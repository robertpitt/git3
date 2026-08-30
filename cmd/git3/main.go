package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robertpitt/git3/internal/config"
	"github.com/robertpitt/git3/internal/engine"
	"github.com/robertpitt/git3/internal/errs"
	gitx "github.com/robertpitt/git3/internal/git"
	"github.com/robertpitt/git3/internal/helper"
	"github.com/robertpitt/git3/internal/locator"
	"github.com/robertpitt/git3/internal/model"
	"github.com/robertpitt/git3/internal/store"
	"github.com/spf13/cobra"
)

var version = "dev"
var commit = "unknown"
var buildTime = "unknown"
var dirty = "true"
var resolvedLogFormat = "human"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	name := filepath.Base(os.Args[0])
	if name == "git-remote-s3" {
		if len(os.Args) != 3 {
			fatal(fmt.Errorf("usage: git-remote-s3 <name> <s3-url>"))
		}
		r, e := repository(ctx, os.Args[2])
		if e != nil {
			fatal(e)
		}
		fatalIf((&helper.Helper{Repo: r, In: os.Stdin, Out: os.Stdout, Err: os.Stderr}).Run(ctx))
		return
	}
	root := command(ctx)
	root.SilenceErrors = true
	root.Use = "git3"
	if name == "git-s3" {
		root.Use = "git s3"
	}
	if e := root.Execute(); e != nil {
		fatal(e)
	}
}
func repository(ctx context.Context, target string) (*engine.Repository, error) {
	g := gitx.Git{}
	raw := target
	if !strings.HasPrefix(target, "s3://") {
		b, e := g.Run(ctx, "config", "--get", "remote."+target+".url")
		if e != nil {
			return nil, fmt.Errorf("resolve remote %s: %w", target, e)
		}
		raw = strings.TrimSpace(string(b))
	}
	l, e := locator.Parse(raw)
	if e != nil {
		return nil, errs.E(errs.ConfigInvalid, "remote URL", e)
	}
	remoteName := ""
	if !strings.HasPrefix(target, "s3://") {
		remoteName = target
	}
	c, e := config.Load(remoteName)
	if e != nil {
		return nil, errs.E(errs.ConfigInvalid, "configuration", e)
	}
	resolvedLogFormat = c.LogFormat
	s, e := store.NewS3(ctx, l, c)
	if e != nil {
		return nil, e
	}
	policy := model.StoragePolicy{ServerSideEncryption: "inherit"}
	if c.SSE == "s3" {
		policy.ServerSideEncryption = "AES256"
	}
	if c.SSE == "kms" {
		policy.ServerSideEncryption = "aws:kms"
		if c.KMSKeyID != "" {
			x := c.KMSKeyID
			policy.KMSKeyID = &x
		}
		policy.BucketKeyEnabled = c.BucketKeyEnabled
	}
	cache := fmt.Sprintf("%x", sha256.Sum256([]byte(l.String())))
	return &engine.Repository{Store: s, Git: g, Version: version, StoragePolicy: policy, DownloadChunkSize: c.DownloadChunkSize, DownloadConcurrency: c.DownloadConcurrency, CacheID: cache, CompactionFanout: c.CompactionFanout, CompactAfterTransactions: c.CompactAfterTransactions, CompactAfterBytes: uint64(c.CompactAfterBytes)}, nil
}
func command(ctx context.Context) *cobra.Command {
	root := &cobra.Command{Use: "git3", SilenceUsage: true}
	root.AddCommand(&cobra.Command{Use: "version", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) {
		fmt.Printf("git3 %s commit=%s buildTime=%s dirty=%s go=%s\n", version, commit, buildTime, dirty, runtime.Version())
	}})
	var asJSON, writeTest bool
	doctor := &cobra.Command{Use: "doctor <remote-or-s3-url>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		r, e := repository(ctx, a[0])
		if e != nil {
			return e
		}
		threshold := r.CompactAfterTransactions
		if threshold < 1 {
			threshold = 32
		}
		rep, e := r.Doctor(ctx, threshold)
		if asJSON {
			_ = json.NewEncoder(os.Stdout).Encode(rep)
		} else if e == nil {
			fmt.Printf("repository=%s format=%s generation=%d revision=%d maintenance_due=%t\n", rep.RepositoryID, rep.ObjectFormat, rep.LogicalGeneration, rep.ManifestRevision, rep.MaintenanceDue)
		}
		if e == nil && writeTest {
			fmt.Fprintln(os.Stderr, "doctor: creating and deleting a scoped write probe")
			e = r.Probe(ctx)
		}
		return e
	}}
	doctor.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	doctor.Flags().BoolVar(&writeTest, "write-test", false, "test scoped object writes")
	root.AddCommand(doctor)
	var full, fsckJSON bool
	fsck := &cobra.Command{Use: "fsck <remote-or-s3-url>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		r, e := repository(ctx, a[0])
		if e != nil {
			return e
		}
		e = r.Fsck(ctx, full)
		if fsckJSON {
			out := map[string]any{"ok": e == nil, "full": full}
			if e != nil {
				out["code"] = errs.CodeOf(e)
				out["error"] = e.Error()
			}
			_ = json.NewEncoder(os.Stdout).Encode(out)
		} else if e == nil {
			fmt.Println("ok")
		}
		return e
	}}
	fsck.Flags().BoolVar(&full, "full", false, "download and verify all live data")
	fsck.Flags().BoolVar(&fsckJSON, "json", false, "machine-readable output")
	root.AddCommand(fsck)
	var maxBytes string
	var all bool
	maint := &cobra.Command{Use: "maintenance <remote-or-s3-url>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		r, e := repository(ctx, a[0])
		if e != nil {
			return e
		}
		if s, se := r.Read(ctx); se == nil {
			fmt.Fprintf(os.Stderr, "maintenance: repositoryId=%s generation=%d floor=%d\n", s.Head.RepositoryID, s.Head.LogicalGeneration, s.Head.Packset.Generation)
		}
		fanout := r.CompactionFanout
		if fanout < 2 {
			fanout = 4
		}
		var maxBytesValue int64
		if maxBytes != "" && !all {
			n, e := config.ParseBytes(maxBytes)
			if e != nil {
				return errs.E(errs.ConfigInvalid, "--max-bytes", e)
			}
			maxBytesValue = n
		}
		id, e := r.Maintenance(ctx, engine.MaintenanceOptions{Fanout: fanout, MaxBytes: maxBytesValue})
		if e == nil {
			fmt.Printf("publicationId=%s\n", id)
		}
		return e
	}}
	maint.Flags().StringVar(&maxBytes, "max-bytes", "", "maximum compaction input")
	maint.Flags().BoolVar(&all, "all", false, "compact all due levels")
	root.AddCommand(maint)
	var execute bool
	var older, resume, abort string
	gc := &cobra.Command{Use: "gc <remote-or-s3-url>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		r, e := repository(ctx, a[0])
		if e != nil {
			return e
		}
		if abort != "" {
			id, e := r.GCAbort(ctx, abort)
			if e == nil {
				fmt.Printf("publicationId=%s\n", id)
			}
			return e
		}
		if resume != "" {
			id, e := r.GCResume(ctx, resume)
			if e == nil {
				fmt.Printf("publicationId=%s\n", id)
			}
			return e
		}
		cut := time.Now().UTC()
		if older != "" {
			cut, e = parseCutoff(older)
			if e != nil {
				return errs.E(errs.ConfigInvalid, "--older-than", e)
			}
		}
		if execute && older == "" {
			return errs.E(errs.ConfigInvalid, "gc", fmt.Errorf("--execute requires --older-than"))
		}
		if execute {
			preview, pe := r.GCDryRun(ctx, cut)
			if pe != nil {
				return pe
			}
			fmt.Fprintf(os.Stderr, "gc: repositoryId=%s cutoff=%s candidates=%d bytes=%d\n", preview.RepositoryID, preview.Cutoff, len(preview.Candidates), preview.TotalBytes)
			id, e := r.GCExecute(ctx, cut)
			if e == nil && asJSON {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "publicationId": id})
			} else if e == nil {
				fmt.Printf("publicationId=%s\n", id)
			}
			return e
		}
		rep, e := r.GCDryRun(ctx, cut)
		if asJSON {
			_ = json.NewEncoder(os.Stdout).Encode(rep)
		} else if e == nil {
			fmt.Printf("repositoryId=%s publicationId=%s etag=%s candidates=%d bytes=%d cutoff=%s\n", rep.RepositoryID, rep.PublicationID, rep.ETag, len(rep.Candidates), rep.TotalBytes, rep.Cutoff)
			for _, c := range rep.Candidates {
				fmt.Printf("%s %d %s %s %s\n", c.Category, c.Size, c.ETag, c.LastModified, c.Key)
			}
		}
		return e
	}}
	gc.Flags().BoolVar(&execute, "execute", false, "publish a barrier and delete candidates")
	gc.Flags().StringVar(&older, "older-than", "", "duration or RFC3339 cutoff")
	gc.Flags().StringVar(&resume, "resume", "", "resume a plan ID")
	gc.Flags().StringVar(&abort, "abort", "", "abort a plan ID")
	gc.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	gc.MarkFlagsMutuallyExclusive("execute", "resume", "abort")
	root.AddCommand(gc)
	root.AddCommand(&cobra.Command{Use: "set-head <remote-or-s3-url> <refs/heads/name>", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, a []string) error {
		r, e := repository(ctx, a[0])
		if e != nil {
			return e
		}
		fmt.Fprintf(os.Stderr, "set-head: target=%s\n", a[1])
		id, e := r.SetHead(ctx, a[1])
		if e == nil {
			fmt.Printf("publicationId=%s\n", id)
		}
		return e
	}})
	return root
}
func parseCutoff(v string) (time.Time, error) {
	if t, e := time.Parse(time.RFC3339, v); e == nil {
		return t, nil
	}
	if len(v) > 1 && (v[len(v)-1] == 'd' || v[len(v)-1] == 'w') {
		n, e := strconv.ParseUint(v[:len(v)-1], 10, 32)
		if e != nil || n > 10000 {
			return time.Time{}, fmt.Errorf("duration outside supported range")
		}
		hours := time.Duration(n) * 24 * time.Hour
		if v[len(v)-1] == 'w' {
			hours *= 7
		}
		return time.Now().UTC().Add(-hours), nil
	}
	d, e := time.ParseDuration(v)
	if e != nil {
		return time.Time{}, e
	}
	if d < 0 {
		return time.Time{}, errors.New("duration must be positive")
	}
	return time.Now().UTC().Add(-d), nil
}
func fatalIf(e error) {
	if e != nil {
		fatal(e)
	}
}
func fatal(e error) {
	code := errs.CodeOf(e)
	if resolvedLogFormat == "json" || os.Getenv("GIT3_LOG_FORMAT") == "json" {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339), "level": "error", "code": code, "message": e.Error(), "git3Version": version})
	} else {
		fmt.Fprintf(os.Stderr, "%s: %v\n", code, e)
	}
	os.Exit(errs.ExitCode(code))
}
