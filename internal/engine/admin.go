package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robertpitt/git3/internal/canonical"
	"github.com/robertpitt/git3/internal/errs"
	gitx "github.com/robertpitt/git3/internal/git"
	"github.com/robertpitt/git3/internal/local"
	"github.com/robertpitt/git3/internal/locator"
	"github.com/robertpitt/git3/internal/model"
	"github.com/robertpitt/git3/internal/store"
)

// DoctorReport contains non-mutating repository health diagnostics.
type DoctorReport struct {
	OK                bool             `json:"ok"`
	RepositoryAbsent  bool             `json:"repositoryAbsent"`
	RepositoryID      string           `json:"repositoryId,omitempty"`
	ObjectFormat      string           `json:"objectFormat,omitempty"`
	LogicalGeneration uint64           `json:"logicalGeneration,omitempty"`
	ManifestRevision  uint64           `json:"manifestRevision,omitempty"`
	PublicationID     string           `json:"publicationId,omitempty"`
	HeadSymref        *string          `json:"headSymref"`
	GCBarrier         *model.GCBarrier `json:"gcBarrier"`
	MaintenanceDue    bool             `json:"maintenanceDue"`
	GitVersion        string           `json:"gitVersion,omitempty"`
	ToolVersion       string           `json:"toolVersion"`
	GoRuntime         string           `json:"goRuntime"`
	RequiredIAM       []string         `json:"requiredIam"`
	CursorStatus      string           `json:"cursorStatus"`
	WALBytes          uint64           `json:"walBytes"`
	MissingFeatures   []string         `json:"missingFeatures"`
	Error             string           `json:"error,omitempty"`
}

// Doctor performs inexpensive local and remote health checks.
func (r *Repository) Doctor(ctx context.Context, threshold int) (DoctorReport, error) {
	v, _ := r.Git.Version(ctx)
	var missing []string
	if x := r.Git.RequireVersion(ctx, 2, 38); x != nil {
		missing = append(missing, x.Error())
	}
	s, e := r.ReadUnconditional(ctx)
	if errors.Is(e, store.ErrNotFound) {
		return DoctorReport{OK: len(missing) == 0, RepositoryAbsent: true, GitVersion: v, ToolVersion: r.Version, GoRuntime: runtime.Version(), RequiredIAM: []string{"s3:GetObject"}, MissingFeatures: missing}, nil
	}
	if e != nil {
		return DoctorReport{GitVersion: v, Error: e.Error()}, e
	}
	var wal uint64
	for _, tx := range s.Transactions {
		if tx.ObjectData != nil {
			wal += tx.ObjectData.Object.Size
		}
	}
	due := int(s.Head.LogicalGeneration-s.Head.Packset.Generation) >= threshold || (r.CompactAfterBytes > 0 && wal >= r.CompactAfterBytes)
	cursor := "not-in-git-repository"
	if ls, x := local.Resolve(ctx, r.Git, s.Head.RepositoryID); x == nil {
		c, x := ls.ReadCursor()
		if x != nil {
			cursor = "missing-or-corrupt"
		} else if c.LogicalGeneration == s.Head.LogicalGeneration && same(c.TransactionID, s.Head.TransactionID) {
			cursor = "current"
		} else if cursorOnChain(s, c) {
			cursor = "behind"
		} else if c.LogicalGeneration > s.Head.LogicalGeneration {
			cursor = "ahead"
		} else {
			cursor = "divergent"
		}
	}
	return DoctorReport{OK: len(missing) == 0, RepositoryID: s.Head.RepositoryID, ObjectFormat: s.Head.ObjectFormat, LogicalGeneration: s.Head.LogicalGeneration, ManifestRevision: s.Head.ManifestRevision, PublicationID: s.Head.PublicationID, HeadSymref: s.Head.HeadSymref, GCBarrier: s.Head.GCBarrier, MaintenanceDue: due, GitVersion: v, ToolVersion: r.Version, GoRuntime: runtime.Version(), RequiredIAM: []string{"s3:GetObject"}, CursorStatus: cursor, WALBytes: wal, MissingFeatures: missing}, nil
}

// Fsck verifies the reachable descriptor and object graph.
func (r *Repository) Fsck(ctx context.Context, full bool) error {
	s, e := r.ReadUnconditional(ctx)
	if e != nil {
		return e
	}
	if e = r.verifyDescriptorGraph(ctx, s); e != nil {
		return e
	}
	ps, e := r.loadPackset(ctx, s)
	if e != nil {
		return e
	}
	for _, l := range ps.Levels {
		for _, p := range l.Packs {
			pm, e := r.Store.Head(ctx, p.Pack.Key)
			if e != nil {
				return e
			}
			im, e := r.Store.Head(ctx, p.Index.Key)
			if e != nil {
				return e
			}
			if uint64(pm.Size) != p.Pack.Size || uint64(im.Size) != p.Index.Size {
				return fmt.Errorf("packset size mismatch")
			}
		}
	}
	if full {
		tmp, e := os.MkdirTemp("", "git3-fsck-")
		if e != nil {
			return e
		}
		defer os.RemoveAll(tmp)
		g := gitx.Git{}
		if _, e = g.Run(ctx, "init", "--bare", "--object-format="+s.Head.ObjectFormat, tmp); e != nil {
			return e
		}
		vr := &Repository{Store: r.Store, Git: gitx.Git{Dir: tmp}, Version: r.Version, DownloadChunkSize: r.DownloadChunkSize, DownloadConcurrency: r.DownloadConcurrency}
		tips := make([]string, 0, len(s.Refs))
		for _, oid := range s.Refs {
			tips = append(tips, oid)
		}
		return vr.fetchState(ctx, s, tips, &FetchReport{}, nil)
	}
	return nil
}
func (r *Repository) verifyDescriptorGraph(ctx context.Context, s *RemoteState) error {
	if !s.logResolved {
		return fmt.Errorf("descriptor verification requires a resolved log")
	}
	check := func(x model.Envelope) error {
		d, e := r.verified(ctx, x.Descriptor, model.MaxTransaction)
		if e != nil {
			return e
		}
		b, e := canonical.Marshal(x.Transaction)
		if e != nil {
			return e
		}
		if !bytes.Equal(d, b) {
			return fmt.Errorf("descriptor/envelope mismatch")
		}
		return nil
	}
	for _, x := range s.logEnvelopes {
		if e := check(x); e != nil {
			return e
		}
	}
	return nil
}

func fileDigest(path string) (int64, string, error) {
	f, e := os.Open(path)
	if e != nil {
		return 0, "", e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, f)
	return n, hex.EncodeToString(h.Sum(nil)), e
}
func (r *Repository) uploadFile(ctx context.Context, key, path string) (model.ObjectRef, error) {
	n, sum, e := fileDigest(path)
	if e != nil {
		return model.ObjectRef{}, e
	}
	f, e := os.Open(path)
	if e != nil {
		return model.ObjectRef{}, e
	}
	defer f.Close()
	_, e = r.Store.Put(ctx, key, f, n, store.PutOptions{IfNoneMatch: true, ContentSHA256: sum})
	if errors.Is(e, store.ErrPrecondition) {
		o, ge := r.Store.Head(ctx, key)
		h := sha256.New()
		if ge == nil && o.Size == n {
			for a := int64(0); a < n; {
				z := a + (64 << 20) - 1
				if z >= n {
					z = n - 1
				}
				if x := r.copyRange(ctx, key, a, z, uint64(n), h); x != nil {
					ge = x
					break
				}
				a = z + 1
			}
		}
		if ge != nil || o.Size != n || hex.EncodeToString(h.Sum(nil)) != sum {
			return model.ObjectRef{}, fmt.Errorf("immutable collision")
		}
		e = nil
	}
	return model.ObjectRef{Key: key, Size: uint64(n), SHA256: sum}, e
}
func copyRefs(in map[string]string) map[string]string {
	o := make(map[string]string, len(in))
	for k, v := range in {
		o[k] = v
	}
	return o
}
func refsAtFloor(s *RemoteState) map[string]string {
	refs := copyRefs(s.Refs)
	for i := len(s.Transactions) - 1; i >= 0; i-- {
		t := s.Transactions[i]
		if t.Generation <= s.Head.Packset.Generation {
			continue
		}
		for j := len(t.Updates) - 1; j >= 0; j-- {
			u := t.Updates[j]
			if u.Old == nil {
				delete(refs, u.Ref)
			} else {
				refs[u.Ref] = *u.Old
			}
		}
	}
	return refs
}

// MaintenanceOptions controls one bounded maintenance operation.
type MaintenanceOptions struct {
	Fanout   int
	MaxBytes int64
}

// Maintenance compacts live objects using a geometric fanout policy.
func (r *Repository) Maintenance(ctx context.Context, options MaintenanceOptions) (string, error) {
	fanout := options.Fanout
	if e := r.Git.RequireVersion(ctx, 2, 38); e != nil {
		return "", e
	}
	s, e := r.ReadUnconditional(ctx)
	if e != nil {
		return "", e
	}
	if s.Head.GCBarrier != nil {
		return "", errs.E(errs.GCBarrierActive, "maintenance", fmt.Errorf("GC barrier active"))
	}
	if e = r.ensureWritePolicy(s); e != nil {
		return "", e
	}
	if e = r.Git.IsComplete(ctx); e != nil {
		return "", e
	}
	ls, e := local.Resolve(ctx, r.Git, s.Head.RepositoryID)
	if e != nil {
		return "", e
	}
	lock, e := ls.Lock(ctx)
	if e != nil {
		return "", e
	}
	defer lock.Close()
	if e = r.Git.VerifyConnectivity(ctx, s.Refs); e != nil {
		return "", e
	}
	old, e := r.loadPackset(ctx, s)
	if e != nil {
		return "", e
	}
	if s.Head.Packset.Generation == s.Head.LogicalGeneration && s.Head.RefSnapshot.Generation == s.Head.LogicalGeneration {
		return s.Head.PublicationID, nil
	}
	targetGen := s.Head.LogicalGeneration
	targetTx := s.Head.TransactionID
	targetRefs := s.Refs
	if options.MaxBytes > 0 {
		refs := refsAtFloor(s)
		targetGen = s.Head.Packset.Generation
		targetTx = s.Head.Packset.TransactionID
		var used uint64
		for _, tx := range s.Transactions {
			if tx.Generation <= s.Head.Packset.Generation {
				continue
			}
			var n uint64
			if tx.ObjectData != nil {
				n = tx.ObjectData.Object.Size
			}
			if used+n > uint64(options.MaxBytes) {
				break
			}
			if e = model.Apply(refs, []model.Transaction{tx}); e != nil {
				return "", e
			}
			used += n
			targetGen = tx.Generation
			x := tx.TransactionID
			targetTx = &x
		}
		if targetGen == s.Head.Packset.Generation {
			return s.Head.PublicationID, nil
		}
		targetRefs = refs
	}
	levels := append([]model.PackLevel(nil), old.Levels...)
	if s.Head.Packset.Generation < targetGen {
		tmp, e := os.MkdirTemp(ls.Root, "maintenance-")
		if e != nil {
			return "", e
		}
		defer os.RemoveAll(tmp)
		var pos, neg []string
		for _, x := range targetRefs {
			pos = append(pos, x)
		}
		for _, x := range refsAtFloor(s) {
			neg = append(neg, x)
		}
		pack, idx, e := r.Git.CreatePack(ctx, tmp, pos, neg)
		if e != nil {
			return "", e
		}
		checksum := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(pack), "pack-"), ".pack")
		pk := ".git/git3/packs/pack-" + checksum + ".pack"
		ik := ".git/git3/packs/pack-" + checksum + ".idx"
		pr, e := r.uploadFile(ctx, pk, pack)
		if e != nil {
			return "", e
		}
		ir, e := r.uploadFile(ctx, ik, idx)
		if e != nil {
			return "", e
		}
		count, e := r.Git.CountPackObjects(ctx, idx)
		if e != nil {
			return "", e
		}
		entry := model.PackEntry{GitPackChecksum: checksum, ObjectCount: count, Pack: pr, Index: ir}
		found := false
		for i := range levels {
			if levels[i].Level == 0 {
				levels[i].Packs = append(levels[i].Packs, entry)
				sort.Slice(levels[i].Packs, func(a, b int) bool { return levels[i].Packs[a].GitPackChecksum < levels[i].Packs[b].GitPackChecksum })
				found = true
			}
		}
		if !found {
			levels = append([]model.PackLevel{{Level: 0, Packs: []model.PackEntry{entry}}}, levels...)
		}
		if fanout < 2 {
			fanout = 4
		}
		for {
			at := -1
			for i := range levels {
				if len(levels[i].Packs) >= fanout {
					at = i
					break
				}
			}
			if at < 0 {
				break
			}
			selected := levels[at]
			names := make([]string, 0, len(selected.Packs))
			for _, p := range selected.Packs {
				pp := filepath.Join(ls.PackDir, "pack-"+p.GitPackChecksum+".pack")
				ip := filepath.Join(ls.PackDir, "pack-"+p.GitPackChecksum+".idx")
				if _, er := os.Stat(pp); er != nil {
					if er = r.downloadVerified(ctx, p.Pack, pp+".part", nil); er != nil {
						return "", er
					}
					if er = os.Rename(pp+".part", pp); er != nil {
						return "", er
					}
				} else if n, sum, x := fileDigest(pp); x != nil || uint64(n) != p.Pack.Size || sum != p.Pack.SHA256 {
					return "", fmt.Errorf("selected local pack failed verification")
				}
				if _, er := os.Stat(ip); er != nil {
					if er = r.downloadVerified(ctx, p.Index, ip+".part", nil); er != nil {
						return "", er
					}
					if er = os.Rename(ip+".part", ip); er != nil {
						return "", er
					}
				} else if n, sum, x := fileDigest(ip); x != nil || uint64(n) != p.Index.Size || sum != p.Index.SHA256 {
					return "", fmt.Errorf("selected local index failed verification")
				}
				if er := verifyTrailer(pp, p.GitPackChecksum); er != nil {
					return "", er
				}
				if er := r.Git.VerifyPack(ctx, ip); er != nil {
					return "", er
				}
				names = append(names, "pack-"+p.GitPackChecksum+".pack")
			}
			mp, mi, er := r.Git.MergePacks(ctx, tmp, names)
			if er != nil {
				return "", er
			}
			sum := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(mp), "merge-"), ".pack")
			mpr, er := r.uploadFile(ctx, ".git/git3/packs/pack-"+sum+".pack", mp)
			if er != nil {
				return "", er
			}
			mir, er := r.uploadFile(ctx, ".git/git3/packs/pack-"+sum+".idx", mi)
			if er != nil {
				return "", er
			}
			count, er := r.Git.CountPackObjects(ctx, mi)
			if er != nil {
				return "", er
			}
			next := selected.Level + 1
			levels = append(levels[:at], levels[at+1:]...)
			added := false
			for i := range levels {
				if levels[i].Level == next {
					levels[i].Packs = append(levels[i].Packs, model.PackEntry{GitPackChecksum: sum, ObjectCount: count, Pack: mpr, Index: mir})
					sort.Slice(levels[i].Packs, func(a, b int) bool { return levels[i].Packs[a].GitPackChecksum < levels[i].Packs[b].GitPackChecksum })
					added = true
				}
			}
			if !added {
				levels = append(levels, model.PackLevel{Level: next, Packs: []model.PackEntry{{GitPackChecksum: sum, ObjectCount: count, Pack: mpr, Index: mir}}})
				sort.Slice(levels, func(i, j int) bool { return levels[i].Level < levels[j].Level })
			}
		}
	}
	packID, snapID := uuid.NewString(), uuid.NewString()
	ps := model.Packset{FormatVersion: 1, RepositoryID: s.Head.RepositoryID, ObjectFormat: s.Head.ObjectFormat, PacksetID: packID, Generation: targetGen, TransactionID: targetTx, Levels: levels}
	if e = ps.Validate(); e != nil {
		return "", e
	}
	psb, e := canonical.Marshal(ps)
	if e != nil {
		return "", e
	}
	psref, e := r.immutable(ctx, ".git/git3/packsets/"+packID+".json", psb)
	if e != nil {
		return "", e
	}
	snap := model.Snapshot{RepositoryID: s.Head.RepositoryID, ObjectFormat: s.Head.ObjectFormat, Generation: s.Head.LogicalGeneration, TransactionID: s.Head.TransactionID, Refs: s.Refs}
	sb, e := snap.MarshalText()
	if e != nil {
		return "", e
	}
	sr, e := r.immutable(ctx, ".git/git3/refs/"+snapID+".refs", sb)
	if e != nil {
		return "", e
	}
	h := s.Head
	h.Packset = model.PacksetPointer{PacksetID: packID, Generation: targetGen, TransactionID: targetTx, Object: psref}
	h.RefSnapshot = model.SnapshotPointer{SnapshotID: snapID, Generation: h.LogicalGeneration, TransactionID: h.TransactionID, Object: sr}
	h.Log.FloorGeneration = targetGen
	h.Log.FloorTransactionID = targetTx
	tail := make([]model.Envelope, 0, len(h.Log.Tail))
	for _, x := range h.Log.Tail {
		if x.Transaction.Generation > targetGen {
			tail = append(tail, x)
		}
	}
	h.Log.Tail = tail
	if h.Log.TipPage != nil && h.Log.TipPage.LastGeneration <= targetGen {
		h.Log.TipPage = nil
	}
	h.ManifestRevision++
	h.PublicationID = uuid.NewString()
	if e = r.validateBootstrap(ctx, h, s.Transactions, s.Refs); e != nil {
		return "", e
	}
	e = r.publishHead(ctx, h, headPublication{
		operation:    "maintenance",
		expectedETag: s.ETag,
		conflict:     "another publisher replaced HEAD",
		confirm: func(observed *RemoteState, _ error) (bool, error) {
			return observed.Head.Packset.PacksetID == packID, nil
		},
	})
	if e != nil {
		return "", e
	}
	return h.PublicationID, nil
}

// SetHead atomically changes the remote symbolic HEAD.
func (r *Repository) SetHead(ctx context.Context, ref string) (string, error) {
	s, e := r.Read(ctx)
	if e != nil {
		return "", e
	}
	if e = r.ensureWritePolicy(s); e != nil {
		return "", e
	}
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", fmt.Errorf("HEAD target must be a branch")
	}
	if _, ok := s.Refs[ref]; !ok {
		return "", fmt.Errorf("branch does not exist")
	}
	h := s.Head
	h.HeadSymref = &ref
	h.ManifestRevision++
	h.PublicationID = uuid.NewString()
	e = r.publishHead(ctx, h, headPublication{operation: "set-head", expectedETag: s.ETag, conflict: "another publisher replaced HEAD"})
	if e != nil {
		return "", e
	}
	return h.PublicationID, nil
}

// GCReport summarizes candidates discovered by garbage collection.
type GCReport struct {
	RepositoryID  string              `json:"repositoryId"`
	PublicationID string              `json:"publicationId"`
	ETag          string              `json:"etag"`
	Cutoff        string              `json:"cutoff"`
	Candidates    []model.GCCandidate `json:"candidates"`
	TotalBytes    uint64              `json:"totalBytes"`
}

func (r *Repository) liveKeys(ctx context.Context, s *RemoteState) (map[string]bool, error) {
	if !s.logResolved {
		return nil, fmt.Errorf("live-key discovery requires a resolved log")
	}
	live := map[string]bool{".git/git3/HEAD": true, s.Head.RefSnapshot.Object.Key: true, s.Head.Packset.Object.Key: true}
	ps, e := r.loadPackset(ctx, s)
	if e != nil {
		return nil, e
	}
	for _, l := range ps.Levels {
		for _, p := range l.Packs {
			live[p.Pack.Key] = true
			live[p.Index.Key] = true
		}
	}
	for _, page := range s.logPages {
		live[page.Key] = true
	}
	for _, e := range s.logEnvelopes {
		live[e.Descriptor.Key] = true
		if e.Transaction.ObjectData != nil {
			live[e.Transaction.ObjectData.Object.Key] = true
		}
	}
	if s.Head.GCBarrier != nil {
		live[s.Head.GCBarrier.Plan.Key] = true
	}
	return live, nil
}

// GCDryRun discovers unreachable objects without changing remote state.
func (r *Repository) GCDryRun(ctx context.Context, cutoff time.Time) (GCReport, error) {
	report, _, e := r.gcDryRun(ctx, cutoff)
	return report, e
}

func (r *Repository) gcDryRun(ctx context.Context, cutoff time.Time) (GCReport, *RemoteState, error) {
	s, e := r.ReadUnconditional(ctx)
	if e != nil {
		return GCReport{}, nil, e
	}
	live, e := r.liveKeys(ctx, s)
	if e != nil {
		return GCReport{}, nil, e
	}
	rep := GCReport{RepositoryID: s.Head.RepositoryID, PublicationID: s.Head.PublicationID, ETag: s.ETag, Cutoff: cutoff.UTC().Format(time.RFC3339)}
	e = r.Store.Walk(ctx, ".git/git3/", func(x store.Metadata) error {
		if e := locator.ValidateManagedKey(x.Key); e != nil {
			return e
		}
		if x.Size < 0 {
			return fmt.Errorf("negative object size for %s", x.Key)
		}
		if live[x.Key] || x.Key == ".git/git3/HEAD" || strings.HasPrefix(x.Key, ".git/git3/probes/") || !x.LastModified.Before(cutoff) {
			return nil
		}
		if len(rep.Candidates) == model.MaxRefs {
			return fmt.Errorf("GC candidate set exceeds %d; execute separately barriered plans", model.MaxRefs)
		}
		c := model.GCCandidate{Key: x.Key, Size: uint64(x.Size), ETag: x.ETag, LastModified: x.LastModified.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z"), Category: gcCategory(x.Key)}
		rep.Candidates = append(rep.Candidates, c)
		rep.TotalBytes += uint64(x.Size)
		return nil
	})
	if e != nil {
		return GCReport{}, nil, e
	}
	sort.Slice(rep.Candidates, func(i, j int) bool { return rep.Candidates[i].Key < rep.Candidates[j].Key })
	return rep, s, nil
}
func gcCategory(k string) string {
	p := strings.TrimPrefix(k, ".git/git3/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}

func (r *Repository) validateGCCandidates(ctx context.Context, s *RemoteState, candidates []model.GCCandidate, allowMissing bool) error {
	live, e := r.liveKeys(ctx, s)
	if e != nil {
		return e
	}
	for _, candidate := range candidates {
		if live[candidate.Key] {
			return fmt.Errorf("candidate became live: %s", candidate.Key)
		}
		metadata, e := r.Store.Head(ctx, candidate.Key)
		if allowMissing && errors.Is(e, store.ErrNotFound) {
			continue
		}
		if e != nil {
			return e
		}
		modified := metadata.LastModified.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
		if metadata.Size < 0 || metadata.ETag != candidate.ETag || uint64(metadata.Size) != candidate.Size || modified != candidate.LastModified {
			return fmt.Errorf("candidate changed: %s", candidate.Key)
		}
	}
	return nil
}

func (r *Repository) deleteGCCandidates(ctx context.Context, candidates []model.GCCandidate, allowMissing bool) error {
	for _, candidate := range candidates {
		e := r.Store.Delete(ctx, candidate.Key, store.DeleteOptions{IfMatch: candidate.ETag})
		if allowMissing && errors.Is(e, store.ErrNotFound) {
			continue
		}
		if e != nil {
			return e
		}
	}
	return nil
}

func (r *Repository) clearGCBarrier(ctx context.Context, s *RemoteState, id, operation string) (string, error) {
	if s.Head.GCBarrier == nil || s.Head.GCBarrier.PlanID != id {
		return "", fmt.Errorf("GC plan is not active")
	}
	h := s.Head
	h.GCBarrier = nil
	h.ManifestRevision++
	h.PublicationID = uuid.NewString()
	if e := r.publishHead(ctx, h, headPublication{operation: operation, expectedETag: s.ETag, conflict: "GC barrier changed"}); e != nil {
		return "", e
	}
	return h.PublicationID, nil
}

// GCExecute publishes a barrier and conditionally deletes planned candidates.
func (r *Repository) GCExecute(ctx context.Context, cutoff time.Time) (string, error) {
	rep, s, e := r.gcDryRun(ctx, cutoff)
	if e != nil {
		return "", e
	}
	if e = r.ensureWritePolicy(s); e != nil {
		return "", e
	}
	if s.Head.GCBarrier != nil {
		return "", errs.E(errs.GCBarrierActive, "gc", fmt.Errorf("GC barrier active"))
	}
	id := uuid.NewString()
	plan := model.GCPlan{FormatVersion: 1, PlanID: id, RepositoryID: rep.RepositoryID, SourcePublicationID: rep.PublicationID, SourceETag: rep.ETag, Cutoff: rep.Cutoff, Candidates: rep.Candidates}
	if e = plan.Validate(); e != nil {
		return "", e
	}
	pb, e := canonical.Marshal(plan)
	if e != nil {
		return "", e
	}
	pr, e := r.immutable(ctx, ".git/git3/gc/"+id+".json", pb)
	if e != nil {
		return "", e
	}
	h := s.Head
	h.GCBarrier = &model.GCBarrier{PlanID: id, CreatedAt: time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z"), Plan: pr}
	h.ManifestRevision++
	h.PublicationID = uuid.NewString()
	e = r.publishHead(ctx, h, headPublication{
		operation:    "gc-start",
		expectedETag: s.ETag,
		conflict:     "another publisher replaced HEAD",
		confirm: func(observed *RemoteState, _ error) (bool, error) {
			return observed.Head.GCBarrier != nil && observed.Head.GCBarrier.PlanID == id, nil
		},
	})
	if e != nil {
		return "", e
	}
	s, e = r.ReadUnconditional(ctx)
	if e != nil {
		return "", e
	}
	if s.Head.GCBarrier == nil || s.Head.GCBarrier.PlanID != id {
		return "", fmt.Errorf("GC barrier changed")
	}
	if e = r.validateGCCandidates(ctx, s, plan.Candidates, false); e != nil {
		return "", e
	}
	if e = r.deleteGCCandidates(ctx, plan.Candidates, false); e != nil {
		return "", e
	}
	return r.clearGCBarrier(ctx, s, id, "gc-finish")
}

// GCAbort clears the identified garbage-collection barrier without deleting candidates.
func (r *Repository) GCAbort(ctx context.Context, id string) (string, error) {
	s, e := r.Read(ctx)
	if e != nil {
		return "", e
	}
	if e = r.ensureWritePolicy(s); e != nil {
		return "", e
	}
	return r.clearGCBarrier(ctx, s, id, "gc-abort")
}

// GCResume continues deletion for the identified garbage-collection plan.
func (r *Repository) GCResume(ctx context.Context, id string) (string, error) {
	s, e := r.ReadUnconditional(ctx)
	if e != nil {
		return "", e
	}
	if e = r.ensureWritePolicy(s); e != nil {
		return "", e
	}
	if s.Head.GCBarrier == nil || s.Head.GCBarrier.PlanID != id {
		return "", fmt.Errorf("GC plan is not active")
	}
	b, e := r.verified(ctx, s.Head.GCBarrier.Plan, 256<<20)
	if e != nil {
		return "", e
	}
	var p model.GCPlan
	if e = canonical.UnmarshalForward(b, &p, 256<<20); e != nil {
		return "", e
	}
	if p.PlanID != id || p.RepositoryID != s.Head.RepositoryID {
		return "", fmt.Errorf("GC plan mismatch")
	}
	if e = p.Validate(); e != nil {
		return "", e
	}
	if e = r.validateGCCandidates(ctx, s, p.Candidates, true); e != nil {
		return "", e
	}
	if e = r.deleteGCCandidates(ctx, p.Candidates, true); e != nil {
		return "", e
	}
	return r.clearGCBarrier(ctx, s, id, "gc-resume")
}

// Probe checks that the configured object-store location is reachable.
func (r *Repository) Probe(ctx context.Context) error {
	id := uuid.NewString()
	key := ".git/git3/probes/" + id
	b := []byte("git3 write probe\n")
	m, e := r.Store.Put(ctx, key, bytes.NewReader(b), int64(len(b)), store.PutOptions{IfNoneMatch: true})
	if e != nil {
		return e
	}
	return r.Store.Delete(ctx, key, store.DeleteOptions{IfMatch: m.ETag})
}
