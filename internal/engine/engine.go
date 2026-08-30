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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/robertpitt/git3/internal/canonical"
	"github.com/robertpitt/git3/internal/errs"
	gitx "github.com/robertpitt/git3/internal/git"
	"github.com/robertpitt/git3/internal/local"
	"github.com/robertpitt/git3/internal/model"
	"github.com/robertpitt/git3/internal/store"
)

// Repository coordinates Git, local state, and object storage for one remote.
type Repository struct {
	Store                    store.Store
	Git                      gitx.Git
	Version                  string
	StoragePolicy            model.StoragePolicy
	DownloadChunkSize        int64
	DownloadConcurrency      int
	CacheID                  string
	CompactionFanout         int
	CompactAfterTransactions int
	CompactAfterBytes        uint64
}

// RemoteState is a validated materialized view of a remote repository.
type RemoteState struct {
	Head         model.Head
	ETag         string
	Refs         map[string]string
	Transactions []model.Transaction
	Cached       bool
	logResolved  bool
	logEnvelopes []model.Envelope
	logPages     []model.ObjectRef
}

// PushCommand describes one ref update requested by Git.
type PushCommand struct {
	Source string
	Dst    string
	NewOID *string
	Force  bool
}

// PushResult reports whether one requested ref update was accepted.
type PushResult struct {
	Dst    string
	OK     bool
	Reason string
}

// Advertisement is the immutable remote view supplied to a fetch or push.
type Advertisement struct {
	ObjectFormat string
	Refs         map[string]string
	HeadSymref   *string
	state        *RemoteState
}

// ProgressUnit identifies how a progress event's values should be displayed.
type ProgressUnit uint8

const (
	ProgressUnitNone ProgressUnit = iota
	ProgressUnitBytes
	ProgressUnitCount
)

// ProgressEvent describes one user-visible phase of a fetch or push.
type ProgressEvent struct {
	Phase   string
	Current uint64
	Total   uint64
	Unit    ProgressUnit
	Done    bool
}

// ProgressSink receives structured engine progress and native Git progress.
type ProgressSink interface {
	io.Writer
	Update(ProgressEvent)
}

// PushOptions controls one push operation.
type PushOptions struct {
	Atomic   bool
	DryRun   bool
	Progress ProgressSink
}

// FetchOptions controls one fetch operation.
type FetchOptions struct {
	Progress ProgressSink
}

// FetchReport contains local side effects and warnings produced by a fetch.
type FetchReport struct {
	Warnings       []string
	InstalledKeeps []string
}

type headPublication struct {
	operation    string
	expectedETag string
	ifAbsent     bool
	conflict     string
	confirm      func(*RemoteState, error) (bool, error)
}

func advertisement(s *RemoteState) *Advertisement {
	refs := make(map[string]string, len(s.Refs))
	for ref, oid := range s.Refs {
		refs[ref] = oid
	}
	var head *string
	if s.Head.HeadSymref != nil {
		value := *s.Head.HeadSymref
		head = &value
	}
	return &Advertisement{ObjectFormat: s.Head.ObjectFormat, Refs: refs, HeadSymref: head, state: s}
}

// Advertise reads and validates the remote state presented to Git.
func (r *Repository) Advertise(ctx context.Context) (*Advertisement, error) {
	s, e := r.Read(ctx)
	if e != nil {
		return nil, e
	}
	return advertisement(s), nil
}

func (r *Repository) publishHead(ctx context.Context, candidate model.Head, publication headPublication) error {
	if e := candidate.Validate(); e != nil {
		return fmt.Errorf("prospective HEAD invalid: %w", e)
	}
	b, e := canonical.Marshal(candidate)
	if e != nil {
		return fmt.Errorf("encode prospective HEAD: %w", e)
	}
	if len(b) > model.MaxHead {
		return fmt.Errorf("prospective HEAD exceeds limit")
	}
	options := store.PutOptions{IfMatch: publication.expectedETag, IfNoneMatch: publication.ifAbsent}
	_, e = r.Store.Put(ctx, ".git/git3/HEAD", bytes.NewReader(b), int64(len(b)), options)
	if errors.Is(e, store.ErrPrecondition) {
		return errs.E(errs.CASConflict, publication.operation, errors.New(publication.conflict))
	}
	if e == nil {
		return nil
	}
	observed, readErr := r.ReadUnconditional(ctx)
	if readErr != nil {
		return errs.E(errs.PublishAmbiguous, publication.operation, e)
	}
	if observed.Head.PublicationID == candidate.PublicationID {
		return nil
	}
	if publication.confirm != nil {
		confirmed, confirmErr := publication.confirm(observed, e)
		if confirmed || confirmErr != nil {
			return confirmErr
		}
	}
	return errs.E(errs.PublishAmbiguous, publication.operation, e)
}

func shaRef(key string, b []byte) model.ObjectRef {
	s := sha256.Sum256(b)
	return model.ObjectRef{Key: key, Size: uint64(len(b)), SHA256: hex.EncodeToString(s[:])}
}
func (r *Repository) immutable(ctx context.Context, key string, b []byte) (model.ObjectRef, error) {
	ref := shaRef(key, b)
	_, e := r.Store.Put(ctx, key, bytes.NewReader(b), int64(len(b)), store.PutOptions{IfNoneMatch: true, ContentSHA256: ref.SHA256})
	if errors.Is(e, store.ErrPrecondition) {
		o, ge := r.Store.Get(ctx, key, "")
		if ge != nil {
			return ref, ge
		}
		s := sha256.Sum256(o.Body)
		if uint64(len(o.Body)) != ref.Size || hex.EncodeToString(s[:]) != ref.SHA256 {
			return ref, errs.E(errs.IntegrityFailed, "immutable-object", fmt.Errorf("collision at %s", key))
		}
		return ref, nil
	}
	return ref, e
}
func (r *Repository) verified(ctx context.Context, ref model.ObjectRef, max int64) ([]byte, error) {
	if e := ref.Validate(); e != nil {
		return nil, e
	}
	if ref.Size > uint64(max) {
		return nil, fmt.Errorf("object %s exceeds limit", ref.Key)
	}
	o, e := r.Store.Get(ctx, ref.Key, "")
	if e != nil {
		return nil, e
	}
	s := sha256.Sum256(o.Body)
	if uint64(len(o.Body)) != ref.Size || hex.EncodeToString(s[:]) != ref.SHA256 {
		return nil, errs.E(errs.IntegrityFailed, "read-object", fmt.Errorf("digest mismatch for %s", ref.Key))
	}
	return o.Body, nil
}

func (r *Repository) copyRange(ctx context.Context, key string, start, end int64, total uint64, dst io.Writer) error {
	part, e := r.Store.OpenRange(ctx, key, start, end)
	if e != nil {
		return e
	}
	want := end - start + 1
	if part.Size != want || part.TotalSize < 0 || uint64(part.TotalSize) != total {
		_ = part.Body.Close()
		return errs.E(errs.IntegrityFailed, "read-range", fmt.Errorf("invalid response metadata for %s", key))
	}
	n, copyErr := io.Copy(dst, io.LimitReader(part.Body, want+1))
	closeErr := part.Body.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != want {
		return errs.E(errs.IntegrityFailed, "read-range", fmt.Errorf("short or oversized response for %s", key))
	}
	return nil
}

func (r *Repository) Read(ctx context.Context) (*RemoteState, error) {
	if r.CacheID != "" {
		if s, used, e := r.readCached(ctx); e != nil {
			return nil, e
		} else if used {
			return s, nil
		}
	}
	o, e := r.Store.Get(ctx, ".git/git3/HEAD", "")
	if e != nil {
		return nil, e
	}
	return r.readObject(ctx, o)
}
func (r *Repository) readCached(ctx context.Context) (*RemoteState, bool, error) {
	repoID, e := local.ReadRemoteMapping(ctx, r.Git, r.CacheID)
	if e != nil {
		return nil, false, nil
	}
	ls, e := local.Resolve(ctx, r.Git, repoID)
	if e != nil {
		return nil, false, nil
	}
	c, e := ls.ReadCursor()
	if e != nil {
		return nil, false, nil
	}
	hb, e := ls.ReadHead()
	if e != nil {
		return nil, false, nil
	}
	rb, e := ls.CachedRefs()
	if e != nil {
		return nil, false, nil
	}
	o, e := r.Store.Get(ctx, ".git/git3/HEAD", c.LastHeadETag)
	if e == nil {
		s, e := r.readObject(ctx, o)
		return s, true, e
	}
	if !errors.Is(e, store.ErrNotModified) {
		return nil, true, e
	}
	var h model.Head
	if e = canonical.UnmarshalForward(hb, &h, model.MaxHead); e != nil || h.Validate() != nil || h.RepositoryID != c.RepositoryID || h.PublicationID != c.LastPublicationID || h.LogicalGeneration != c.LogicalGeneration || !same(h.TransactionID, c.TransactionID) {
		return nil, false, nil
	}
	snap, e := model.ParseSnapshot(rb)
	if e != nil || snap.RepositoryID != h.RepositoryID || snap.Generation != h.LogicalGeneration || !same(snap.TransactionID, h.TransactionID) {
		return nil, false, nil
	}
	return &RemoteState{Head: h, ETag: c.LastHeadETag, Refs: snap.Refs, Cached: true}, true, nil
}
func (r *Repository) readObject(ctx context.Context, o store.Object) (*RemoteState, error) {
	var e error
	if len(o.Body) > model.MaxHead {
		return nil, errs.E(errs.RepositoryCorrupt, "read-head", fmt.Errorf("HEAD too large"))
	}
	var h model.Head
	if e = canonical.UnmarshalForward(o.Body, &h, model.MaxHead); e != nil {
		return nil, errs.E(errs.RepositoryCorrupt, "read-head", e)
	}
	if e = h.Validate(); e != nil {
		if errors.Is(e, model.ErrFormatUnsupported) {
			return nil, errs.E(errs.FormatUnsupported, "read-head", e)
		}
		return nil, errs.E(errs.RepositoryCorrupt, "read-head", e)
	}
	log, e := r.collectTransactions(ctx, h)
	if e != nil {
		return nil, errs.WithDefault(errs.RepositoryCorrupt, "read-log", e)
	}
	refs, e := r.reconstruct(ctx, h, log.transactions)
	if e != nil {
		return nil, errs.WithDefault(errs.RepositoryCorrupt, "read-refs", e)
	}
	return &RemoteState{Head: h, ETag: o.ETag, Refs: refs, Transactions: log.transactions, logResolved: true, logEnvelopes: log.envelopes, logPages: log.pages}, nil
}

type resolvedLog struct {
	transactions []model.Transaction
	envelopes    []model.Envelope
	pages        []model.ObjectRef
}

func (r *Repository) collectTransactions(ctx context.Context, h model.Head) (resolvedLog, error) {
	var pages [][]model.Envelope
	var pageObjects []model.ObjectRef
	seen := map[string]bool{}
	p := h.Log.TipPage
	for p != nil && p.LastGeneration > h.Log.FloorGeneration {
		if _, e := uuid.Parse(p.PageID); e != nil || p.FirstGeneration > p.LastGeneration {
			return resolvedLog{}, fmt.Errorf("invalid log page pointer")
		}
		if seen[p.PageID] {
			return resolvedLog{}, fmt.Errorf("log page cycle")
		}
		seen[p.PageID] = true
		b, e := r.verified(ctx, p.Object, model.MaxLogPage)
		if e != nil {
			return resolvedLog{}, e
		}
		var page model.LogPage
		if e = canonical.UnmarshalForward(b, &page, model.MaxLogPage); e != nil {
			return resolvedLog{}, e
		}
		if page.FormatVersion != 1 || page.RepositoryID != h.RepositoryID || page.PageID != p.PageID || len(page.Transactions) == 0 || len(page.Transactions) > 32 {
			return resolvedLog{}, fmt.Errorf("invalid log page")
		}
		if page.FirstGeneration != page.Transactions[0].Transaction.Generation || page.LastGeneration != page.Transactions[len(page.Transactions)-1].Transaction.Generation {
			return resolvedLog{}, fmt.Errorf("log page bounds mismatch")
		}
		first := page.Transactions[0].Transaction
		if page.BaseGeneration != first.ParentGeneration || !same(page.BaseTransactionID, first.ParentTransactionID) {
			return resolvedLog{}, fmt.Errorf("log page base mismatch")
		}
		if page.Previous != nil && (page.Previous.LastGeneration != page.BaseGeneration || page.Previous.FirstGeneration > page.Previous.LastGeneration) {
			return resolvedLog{}, fmt.Errorf("log page previous mismatch")
		}
		pages = append(pages, page.Transactions)
		pageObjects = append(pageObjects, p.Object)
		if page.Previous == nil {
			p = nil
		} else {
			p = &model.PagePointer{PageID: page.Previous.PageID, FirstGeneration: page.Previous.FirstGeneration, LastGeneration: page.Previous.LastGeneration, Object: page.Previous.Object}
		}
	}
	var envs []model.Envelope
	for i := len(pages) - 1; i >= 0; i-- {
		envs = append(envs, pages[i]...)
	}
	envs = append(envs, h.Log.Tail...)
	var out []model.Transaction
	var liveEnvelopes []model.Envelope
	gen := h.Log.FloorGeneration
	pid := h.Log.FloorTransactionID
	for _, e := range envs {
		t := e.Transaction
		if t.Generation <= h.Log.FloorGeneration {
			continue
		}
		if er := t.Validate(); er != nil {
			return resolvedLog{}, er
		}
		if t.RepositoryID != h.RepositoryID || t.ObjectFormat != h.ObjectFormat || t.Generation != gen+1 || !same(pid, t.ParentTransactionID) {
			return resolvedLog{}, fmt.Errorf("transaction chain gap at %d", t.Generation)
		}
		if er := e.Descriptor.Validate(); er != nil {
			return resolvedLog{}, er
		}
		out = append(out, t)
		liveEnvelopes = append(liveEnvelopes, e)
		gen = t.Generation
		x := t.TransactionID
		pid = &x
	}
	if gen != h.LogicalGeneration || !same(pid, h.TransactionID) {
		return resolvedLog{}, fmt.Errorf("transaction chain does not reach HEAD")
	}
	return resolvedLog{transactions: out, envelopes: liveEnvelopes, pages: pageObjects}, nil
}
func same(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
func (r *Repository) reconstruct(ctx context.Context, h model.Head, txs []model.Transaction) (map[string]string, error) {
	b, e := r.verified(ctx, h.RefSnapshot.Object, model.MaxSnapshot)
	if e != nil {
		return nil, e
	}
	s, e := model.ParseSnapshot(b)
	if e != nil {
		return nil, e
	}
	if s.RepositoryID != h.RepositoryID || s.ObjectFormat != h.ObjectFormat || s.Generation != h.RefSnapshot.Generation || !same(s.TransactionID, h.RefSnapshot.TransactionID) {
		return nil, fmt.Errorf("snapshot pointer mismatch")
	}
	for _, t := range txs {
		if t.Generation > s.Generation {
			if e = model.Apply(s.Refs, []model.Transaction{t}); e != nil {
				return nil, e
			}
		}
	}
	if h.HeadSymref != nil {
		if _, ok := s.Refs[*h.HeadSymref]; !ok {
			return nil, fmt.Errorf("HEAD target absent")
		}
	}
	return s.Refs, nil
}

// Push validates, uploads, and atomically publishes a set of ref updates.
func (r *Repository) Push(ctx context.Context, advertised *Advertisement, cmds []PushCommand, options PushOptions) ([]PushResult, error) {
	if e := r.Git.RequireVersion(ctx, 2, 38); e != nil {
		return nil, e
	}
	resolved, e := r.resolvePushCommands(ctx, cmds)
	if e != nil {
		return nil, e
	}
	cmds = resolved
	var state *RemoteState
	if advertised != nil {
		state = advertised.state
	}
	if state == nil {
		state, e = r.Read(ctx)
		if errors.Is(e, store.ErrNotFound) {
			return r.initialize(ctx, cmds, options)
		}
		if e != nil {
			return nil, e
		}
	}
	if state.Cached {
		advertisedETag := state.ETag
		verified, e := r.ReadUnconditional(ctx)
		if e != nil {
			return nil, e
		}
		if verified.ETag != advertisedETag {
			return nil, errs.E(errs.CASConflict, "push", fmt.Errorf("remote changed since ref advertisement"))
		}
		state = verified
	}
	format, e := r.Git.ObjectFormat(ctx)
	if e != nil {
		return nil, e
	}
	if format != state.Head.ObjectFormat {
		return nil, errs.E(errs.FormatUnsupported, "push", fmt.Errorf("object format mismatch: local %s remote %s", format, state.Head.ObjectFormat))
	}
	if r.StoragePolicy.ServerSideEncryption != "" && !policyEqual(r.StoragePolicy, state.Head.StoragePolicy) {
		return nil, fmt.Errorf("local encryption settings conflict with repository storage policy")
	}
	updates, res, e := r.validatePush(ctx, state, cmds)
	if e != nil {
		return nil, e
	}
	if rejectAtomicBatch(res, options.Atomic) {
		return res, nil
	}
	if len(updates) == 0 {
		return res, nil
	}
	if options.DryRun {
		return res, nil
	}
	tx, e := r.makeTransaction(ctx, state, updates, options.Progress)
	if e != nil {
		return res, e
	}
	env, e := r.storeTransaction(ctx, &tx)
	if e != nil {
		return res, e
	}
	candidate := state.Head
	candidate.LogicalGeneration = tx.Generation
	candidate.TransactionID = &tx.TransactionID
	candidate.ManifestRevision++
	candidate.PublicationID = uuid.NewString()
	if candidate.HeadSymref != nil {
		for _, u := range updates {
			if u.Ref == *candidate.HeadSymref && u.New == nil {
				candidate.HeadSymref = nil
			}
		}
	}
	if e = r.appendEnvelope(ctx, &candidate, env); e != nil {
		return res, e
	}
	if options.Progress != nil {
		options.Progress.Update(ProgressEvent{Phase: "Publishing refs"})
	}
	e = r.publishHead(ctx, candidate, headPublication{
		operation:    "publish",
		expectedETag: state.ETag,
		conflict:     "another writer replaced HEAD",
		confirm: func(observed *RemoteState, publishErr error) (bool, error) {
			for _, x := range observed.Transactions {
				if x.Generation == tx.Generation {
					a, _ := canonical.Marshal(x)
					b, _ := canonical.Marshal(tx)
					if bytes.Equal(a, b) {
						return true, nil
					}
					return false, errs.E(errs.CASConflict, "publish", fmt.Errorf("generation %d belongs to another transaction", tx.Generation))
				}
			}
			if observed.Head.LogicalGeneration <= state.Head.LogicalGeneration {
				return false, publishErr
			}
			return false, nil
		},
	})
	if e != nil {
		return res, e
	}
	if options.Progress != nil {
		options.Progress.Update(ProgressEvent{Phase: "Publishing refs", Done: true})
	}
	return res, nil
}

func (r *Repository) resolvePushCommands(ctx context.Context, cmds []PushCommand) ([]PushCommand, error) {
	resolved := append([]PushCommand(nil), cmds...)
	for i := range resolved {
		if resolved[i].Source == "" || resolved[i].NewOID != nil {
			continue
		}
		oid, e := r.Git.Resolve(ctx, resolved[i].Source)
		if e != nil {
			oid = resolved[i].Source
		}
		resolved[i].NewOID = &oid
	}
	return resolved, nil
}

func rejectAtomicBatch(results []PushResult, atomic bool) bool {
	if !atomic {
		return false
	}
	bad := false
	for _, result := range results {
		if !result.OK {
			bad = true
			break
		}
	}
	if !bad {
		return false
	}
	for i := range results {
		results[i].OK = false
		if results[i].Reason == "" {
			results[i].Reason = "atomic push rejected"
		}
	}
	return true
}
func policyEqual(a, b model.StoragePolicy) bool {
	if a.ServerSideEncryption != b.ServerSideEncryption {
		return false
	}
	if !same(a.KMSKeyID, b.KMSKeyID) {
		return false
	}
	return (a.BucketKeyEnabled == nil && b.BucketKeyEnabled == nil) || (a.BucketKeyEnabled != nil && b.BucketKeyEnabled != nil && *a.BucketKeyEnabled == *b.BucketKeyEnabled)
}
func (r *Repository) ensureWritePolicy(s *RemoteState) error {
	if r.StoragePolicy.ServerSideEncryption != "" && !policyEqual(r.StoragePolicy, s.Head.StoragePolicy) {
		return fmt.Errorf("local encryption settings conflict with repository storage policy")
	}
	return nil
}
func (r *Repository) validatePush(ctx context.Context, s *RemoteState, cmds []PushCommand) ([]model.Update, []PushResult, error) {
	oids := make([]string, 0, len(cmds))
	for _, c := range cmds {
		if c.NewOID != nil && model.ValidOID(*c.NewOID, s.Head.ObjectFormat) {
			oids = append(oids, *c.NewOID)
		}
	}
	existing, e := r.Git.ExistingObjects(ctx, oids)
	if e != nil {
		return nil, nil, e
	}
	seen := map[string]bool{}
	var us []model.Update
	res := make([]PushResult, len(cmds))
	for i, c := range cmds {
		res[i] = PushResult{Dst: c.Dst}
		fail := func(x string) { res[i].Reason = x }
		if seen[c.Dst] {
			fail("duplicate destination")
			continue
		}
		seen[c.Dst] = true
		if r.Git.CheckRef(ctx, c.Dst) != nil || c.Dst == "HEAD" {
			fail("invalid destination ref")
			continue
		}
		old, exists := s.Refs[c.Dst]
		if c.NewOID == nil {
			if !exists {
				res[i].OK = true
				continue
			}
			o := old
			us = append(us, model.Update{Ref: c.Dst, Old: &o, New: nil, Kind: "delete"})
			res[i].OK = true
			continue
		}
		if !model.ValidOID(*c.NewOID, s.Head.ObjectFormat) || !existing[*c.NewOID] {
			fail("source object missing or wrong format")
			continue
		}
		if exists && old == *c.NewOID {
			res[i].OK = true
			continue
		}
		kind := "create"
		var op *string
		if exists {
			o := old
			op = &o
			ancestor := false
			if strings.HasPrefix(c.Dst, "refs/heads/") {
				ancestor, e = r.Git.IsAncestor(ctx, old, *c.NewOID)
				if e != nil {
					return nil, nil, e
				}
			}
			if ancestor {
				kind = "fast-forward"
			} else if c.Force {
				kind = "force"
			} else {
				fail("non-fast-forward update requires force")
				continue
			}
		}
		n := *c.NewOID
		us = append(us, model.Update{Ref: c.Dst, Old: op, New: &n, Kind: kind})
		res[i].OK = true
	}
	sort.Slice(us, func(i, j int) bool { return us[i].Ref < us[j].Ref })
	return us, res, nil
}

type trailerWriter struct {
	h    io.Writer
	n    int64
	tail []byte
	want int
}

func (t *trailerWriter) Write(p []byte) (int, error) {
	n, e := t.h.Write(p)
	t.n += int64(n)
	written := p[:n]
	if len(written) >= t.want {
		if cap(t.tail) < t.want {
			t.tail = make([]byte, t.want)
		} else {
			t.tail = t.tail[:t.want]
		}
		copy(t.tail, written[len(written)-t.want:])
		return n, e
	}
	overflow := len(t.tail) + len(written) - t.want
	if overflow > 0 {
		copy(t.tail, t.tail[overflow:])
		t.tail = t.tail[:len(t.tail)-overflow]
	}
	t.tail = append(t.tail, written...)
	return n, e
}
func (r *Repository) makeTransaction(ctx context.Context, s *RemoteState, updates []model.Update, progress ProgressSink) (model.Transaction, error) {
	tx := model.Transaction{FormatVersion: 1, RepositoryID: s.Head.RepositoryID, ObjectFormat: s.Head.ObjectFormat, Generation: s.Head.LogicalGeneration + 1, ParentGeneration: s.Head.LogicalGeneration, ParentTransactionID: s.Head.TransactionID, TransactionID: uuid.NewString(), CreatedAt: time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z"), WriterVersion: r.Version, Updates: updates}
	var pos []string
	for _, u := range updates {
		if u.New != nil {
			pos = append(pos, *u.New)
		}
	}
	if len(pos) == 0 {
		return tx, nil
	}
	var neg []string
	remoteOIDs := make([]string, 0, len(s.Refs))
	for _, oid := range s.Refs {
		remoteOIDs = append(remoteOIDs, oid)
	}
	existing, e := r.Git.ExistingObjects(ctx, remoteOIDs)
	if e != nil {
		return tx, e
	}
	for _, oid := range remoteOIDs {
		if existing[oid] {
			neg = append(neg, oid)
		}
	}
	pr, e := r.Git.PackObjects(ctx, pos, neg, true, progress)
	if e != nil {
		return tx, e
	}
	defer pr.Close()
	sum := sha256.New()
	want := 20
	if tx.ObjectFormat == "sha256" {
		want = 32
	}
	tw := &trailerWriter{h: sum, want: want}
	reader := io.TeeReader(pr, tw)
	key := ".git/git3/wal/" + tx.TransactionID + ".pack"
	_, e = r.Store.Put(ctx, key, reader, -1, store.PutOptions{IfNoneMatch: true})
	if e != nil {
		_ = pr.Close()
		_ = pr.Wait()
		return tx, e
	}
	if e = pr.Wait(); e != nil {
		return tx, e
	}
	if progress != nil {
		progress.Update(ProgressEvent{Phase: "Finalizing S3 upload", Done: true})
	}
	if len(tw.tail) != want {
		return tx, fmt.Errorf("short pack")
	}
	tx.ObjectData = &model.ObjectData{Object: model.ObjectRef{Key: key, Size: uint64(tw.n), SHA256: hex.EncodeToString(sum.Sum(nil))}, GitPackChecksum: hex.EncodeToString(tw.tail), Thin: true, BaseGeneration: s.Head.LogicalGeneration, BaseTransactionID: s.Head.TransactionID}
	return tx, nil
}
func (r *Repository) storeTransaction(ctx context.Context, tx *model.Transaction) (model.Envelope, error) {
	b, e := canonical.Marshal(tx)
	if e != nil {
		return model.Envelope{}, e
	}
	key := fmt.Sprintf(".git/git3/transactions/%020d-%s.json", tx.Generation, tx.TransactionID)
	ref, e := r.immutable(ctx, key, b)
	return model.Envelope{Descriptor: ref, Transaction: *tx}, e
}
func (r *Repository) appendEnvelope(ctx context.Context, h *model.Head, e model.Envelope) error {
	proposed := append(append([]model.Envelope(nil), h.Log.Tail...), e)
	b, er := canonical.Marshal(proposed)
	if er != nil {
		return er
	}
	if len(proposed) <= 32 && len(b) <= 1<<20 {
		h.Log.Tail = proposed
		return nil
	}
	id := uuid.NewString()
	base := proposed[0].Transaction.ParentGeneration
	bt := proposed[0].Transaction.ParentTransactionID
	page := model.LogPage{FormatVersion: 1, RepositoryID: h.RepositoryID, PageID: id, BaseGeneration: base, BaseTransactionID: bt, FirstGeneration: proposed[0].Transaction.Generation, LastGeneration: proposed[len(proposed)-1].Transaction.Generation, Transactions: proposed}
	if h.Log.TipPage != nil {
		page.Previous = &model.PagePrevious{PageID: h.Log.TipPage.PageID, FirstGeneration: h.Log.TipPage.FirstGeneration, LastGeneration: h.Log.TipPage.LastGeneration, Object: h.Log.TipPage.Object}
	}
	pb, er := canonical.Marshal(page)
	if er != nil {
		return er
	}
	ref, er := r.immutable(ctx, ".git/git3/log-pages/"+id+".json", pb)
	if er != nil {
		return er
	}
	h.Log.TipPage = &model.PagePointer{PageID: id, FirstGeneration: page.FirstGeneration, LastGeneration: page.LastGeneration, Object: ref}
	h.Log.Tail = []model.Envelope{}
	return nil
}

func (r *Repository) initialize(ctx context.Context, cmds []PushCommand, options PushOptions) ([]PushResult, error) {
	format, e := r.Git.ObjectFormat(ctx)
	if e != nil {
		return nil, e
	}
	repoID := uuid.NewString()
	zero := &RemoteState{Head: model.Head{RepositoryID: repoID, ObjectFormat: format, LogicalGeneration: 0, TransactionID: nil}, Refs: map[string]string{}}
	updates, res, e := r.validatePush(ctx, zero, cmds)
	if e != nil {
		return nil, e
	}
	if rejectAtomicBatch(res, options.Atomic) {
		return res, nil
	}
	branches := []string{}
	for _, u := range updates {
		if u.New != nil && strings.HasPrefix(u.Ref, "refs/heads/") {
			branches = append(branches, u.Ref)
		}
	}
	if len(branches) == 0 {
		return res, fmt.Errorf("initial push must create a branch")
	}
	var headRef string
	if len(branches) == 1 {
		headRef = branches[0]
	} else {
		localHead, er := r.Git.SymbolicHEAD(ctx)
		if er != nil {
			return res, fmt.Errorf("ambiguous initial default branch")
		}
		matches := 0
		for _, c := range cmds {
			if c.NewOID != nil && (c.Source == localHead || c.Source == "HEAD") {
				headRef = c.Dst
				matches++
			}
		}
		if headRef == "" || matches != 1 {
			return res, fmt.Errorf("ambiguous initial default branch")
		}
	}
	if options.DryRun {
		return res, nil
	}
	snapID, packID := uuid.NewString(), uuid.NewString()
	snap := model.Snapshot{RepositoryID: repoID, ObjectFormat: format, Refs: map[string]string{}}
	sb, e := snap.MarshalText()
	if e != nil {
		return res, e
	}
	sref, e := r.immutable(ctx, ".git/git3/refs/"+snapID+".refs", sb)
	if e != nil {
		return res, e
	}
	ps := model.Packset{FormatVersion: 1, RepositoryID: repoID, ObjectFormat: format, PacksetID: packID, Generation: 0, Levels: []model.PackLevel{}}
	pb, e := canonical.Marshal(ps)
	if e != nil {
		return res, e
	}
	pref, e := r.immutable(ctx, ".git/git3/packsets/"+packID+".json", pb)
	if e != nil {
		return res, e
	}
	policy := r.StoragePolicy
	if policy.ServerSideEncryption == "" {
		policy = model.StoragePolicy{ServerSideEncryption: "inherit"}
	}
	head := model.Head{FormatVersion: 1, RequiredFeatures: []string{}, RepositoryID: repoID, ObjectFormat: format, ManifestRevision: 1, PublicationID: uuid.NewString(), HeadSymref: &headRef, StoragePolicy: policy, RefSnapshot: model.SnapshotPointer{SnapshotID: snapID, Object: sref}, Packset: model.PacksetPointer{PacksetID: packID, Object: pref}, Log: model.Log{Tail: []model.Envelope{}}}
	zero.Head = head
	tx, e := r.makeTransaction(ctx, zero, updates, options.Progress)
	if e != nil {
		return res, e
	}
	env, e := r.storeTransaction(ctx, &tx)
	if e != nil {
		return res, e
	}
	head.LogicalGeneration = 1
	head.TransactionID = &tx.TransactionID
	head.Log.Tail = []model.Envelope{env}
	prospectiveRefs := map[string]string{}
	if e = model.Apply(prospectiveRefs, []model.Transaction{tx}); e != nil {
		return res, e
	}
	if options.Progress != nil {
		options.Progress.Update(ProgressEvent{Phase: "Verifying uploaded objects"})
	}
	if e = r.validateBootstrap(ctx, head, []model.Transaction{tx}, prospectiveRefs); e != nil {
		return res, e
	}
	if options.Progress != nil {
		options.Progress.Update(ProgressEvent{Phase: "Verifying uploaded objects", Done: true})
		options.Progress.Update(ProgressEvent{Phase: "Publishing refs"})
	}
	e = r.publishHead(ctx, head, headPublication{
		operation: "initialize",
		ifAbsent:  true,
		conflict:  "repository was initialized concurrently",
		confirm: func(observed *RemoteState, _ error) (bool, error) {
			for _, x := range observed.Transactions {
				if x.Generation == 1 && x.TransactionID == tx.TransactionID {
					return true, nil
				}
			}
			return false, errs.E(errs.CASConflict, "initialize", fmt.Errorf("another initialization was published"))
		},
	})
	if e != nil {
		return res, e
	}
	if options.Progress != nil {
		options.Progress.Update(ProgressEvent{Phase: "Publishing refs", Done: true})
	}
	return res, nil
}
func (r *Repository) validateBootstrap(ctx context.Context, h model.Head, txs []model.Transaction, refs map[string]string) error {
	tmp, e := os.MkdirTemp("", "git3-bootstrap-check-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	g := gitx.Git{}
	if _, e = g.Run(ctx, "init", "--bare", "--object-format="+h.ObjectFormat, tmp); e != nil {
		return e
	}
	vr := &Repository{Store: r.Store, Git: gitx.Git{Dir: tmp}, Version: r.Version, DownloadChunkSize: r.DownloadChunkSize, DownloadConcurrency: r.DownloadConcurrency}
	tips := make([]string, 0, len(refs))
	for _, oid := range refs {
		tips = append(tips, oid)
	}
	return vr.fetchState(ctx, &RemoteState{Head: h, Refs: refs, Transactions: txs}, tips, &FetchReport{}, nil)
}

// Fetch installs requested objects and verifies repository connectivity.
func (r *Repository) Fetch(ctx context.Context, advertised *Advertisement, requested []string, options FetchOptions) (FetchReport, error) {
	var report FetchReport
	if advertised == nil || advertised.state == nil {
		return report, fmt.Errorf("fetch requires an advertisement")
	}
	e := r.fetchState(ctx, advertised.state, requested, &report, options.Progress)
	return report, e
}

func (r *Repository) fetchState(ctx context.Context, s *RemoteState, requested []string, report *FetchReport, progress ProgressSink) error {
	if e := r.Git.RequireVersion(ctx, 2, 38); e != nil {
		return e
	}
	ls, e := local.Resolve(ctx, r.Git, s.Head.RepositoryID)
	if e != nil {
		return e
	}
	lock, e := ls.Lock(ctx)
	if e != nil {
		return e
	}
	defer lock.Close()
	cursor, cursorErr := ls.ReadCursor()
	if cursorErr == nil {
		if cursor.LogicalGeneration > s.Head.LogicalGeneration || (cursor.LogicalGeneration == s.Head.LogicalGeneration && !same(cursor.TransactionID, s.Head.TransactionID)) {
			report.Warnings = append(report.Warnings, "remote logical history is lower or divergent; retaining local objects and bootstrapping")
		}
	}
	if cursorErr == nil && cursor.LogicalGeneration == s.Head.LogicalGeneration && same(cursor.TransactionID, s.Head.TransactionID) {
		existing, probeErr := r.Git.ExistingObjects(ctx, requested)
		if probeErr != nil {
			return probeErr
		}
		ok := true
		for _, oid := range requested {
			if !existing[oid] {
				ok = false
			}
		}
		if ok {
			if cursor.LastHeadETag != s.ETag || cursor.LastPublicationID != s.Head.PublicationID {
				return r.writeCursor(ctx, ls, s)
			}
			return nil
		}
	}
	if s.Cached {
		var e error
		s, e = r.ReadUnconditional(ctx)
		if e != nil {
			return e
		}
	}
	if r.Git.VerifyConnectivity(ctx, s.Refs) == nil {
		return r.writeCursor(ctx, ls, s)
	}
	if cursorErr == nil && cursor.LogicalGeneration >= s.Head.Log.FloorGeneration && cursor.LogicalGeneration < s.Head.LogicalGeneration && cursorOnChain(s, cursor) {
		var pending []model.ObjectData
		for _, t := range s.Transactions {
			if t.Generation > cursor.LogicalGeneration && t.ObjectData != nil {
				pending = append(pending, *t.ObjectData)
			}
		}
		ok := true
		for i, data := range pending {
			if e = r.installWAL(ctx, data, report, progress); e != nil {
				ok = false
				break
			}
			if progress != nil {
				progress.Update(ProgressEvent{Phase: "Applying transaction packs", Current: uint64(i + 1), Total: uint64(len(pending)), Unit: ProgressUnitCount})
			}
		}
		if ok {
			if progress != nil && len(pending) > 0 {
				progress.Update(ProgressEvent{Phase: "Applying transaction packs", Current: uint64(len(pending)), Total: uint64(len(pending)), Unit: ProgressUnitCount, Done: true})
				progress.Update(ProgressEvent{Phase: "Verifying object connectivity"})
			}
			if r.Git.VerifyConnectivity(ctx, s.Refs) == nil {
				if progress != nil && len(pending) > 0 {
					progress.Update(ProgressEvent{Phase: "Verifying object connectivity", Done: true})
				}
				return r.writeCursor(ctx, ls, s)
			}
		}
	}
	installedPackset, e := r.installPackset(ctx, ls, s, progress)
	if e != nil {
		return e
	}
	var pending []model.ObjectData
	for _, t := range s.Transactions {
		if t.Generation > s.Head.Packset.Generation && t.ObjectData != nil {
			pending = append(pending, *t.ObjectData)
		}
	}
	for i, data := range pending {
		if e = r.installWAL(ctx, data, report, progress); e != nil {
			return e
		}
		if progress != nil {
			progress.Update(ProgressEvent{Phase: "Applying transaction packs", Current: uint64(i + 1), Total: uint64(len(pending)), Unit: ProgressUnitCount})
		}
	}
	if progress != nil && len(pending) > 0 {
		progress.Update(ProgressEvent{Phase: "Applying transaction packs", Current: uint64(len(pending)), Total: uint64(len(pending)), Unit: ProgressUnitCount, Done: true})
	}
	didInstall := installedPackset || len(pending) > 0
	if progress != nil && didInstall {
		progress.Update(ProgressEvent{Phase: "Verifying object connectivity"})
	}
	if e = r.Git.VerifyConnectivity(ctx, s.Refs); e != nil {
		return e
	}
	if progress != nil && didInstall {
		progress.Update(ProgressEvent{Phase: "Verifying object connectivity", Done: true})
	}
	return r.writeCursor(ctx, ls, s)
}
func cursorOnChain(s *RemoteState, c model.Cursor) bool {
	if c.RepositoryID != s.Head.RepositoryID || c.ObjectFormat != s.Head.ObjectFormat {
		return false
	}
	if c.LogicalGeneration == s.Head.Log.FloorGeneration {
		return same(c.TransactionID, s.Head.Log.FloorTransactionID)
	}
	for _, t := range s.Transactions {
		if t.Generation == c.LogicalGeneration {
			return c.TransactionID != nil && *c.TransactionID == t.TransactionID
		}
	}
	return false
}
func (r *Repository) installWAL(ctx context.Context, d model.ObjectData, report *FetchReport, progress ProgressSink) error {
	if e := d.Object.Validate(); e != nil {
		return e
	}
	pr, pw := io.Pipe()
	go func() {
		h := sha256.New()
		chunk := r.DownloadChunkSize
		if chunk <= 0 {
			chunk = 64 << 20
		}
		var total uint64
		for a := int64(0); uint64(a) < d.Object.Size; {
			b := a + chunk - 1
			if uint64(b) >= d.Object.Size {
				b = int64(d.Object.Size) - 1
			}
			want := b - a + 1
			if e := r.copyRange(ctx, d.Object.Key, a, b, d.Object.Size, io.MultiWriter(h, pw)); e != nil {
				pw.CloseWithError(e)
				return
			}
			total += uint64(want)
			a = b + 1
		}
		if total != d.Object.Size || hex.EncodeToString(h.Sum(nil)) != d.Object.SHA256 {
			pw.CloseWithError(errs.E(errs.IntegrityFailed, "install-wal", fmt.Errorf("WAL digest mismatch")))
			return
		}
		pw.Close()
	}()
	out, e := r.Git.IndexPack(ctx, pr, true, "git3-fetch", progress)
	if e != nil {
		return e
	}
	fields := strings.Fields(out)
	if len(fields) > 0 {
		hash := fields[len(fields)-1]
		if model.ValidOID(hash, d.ObjectFormatGuess()) {
			if dir, de := r.Git.PackDir(ctx); de == nil {
				p := filepath.Join(dir, "pack-"+hash+".keep")
				if _, se := os.Stat(p); se == nil {
					report.InstalledKeeps = append(report.InstalledKeeps, p)
				}
			}
		}
	}
	return nil
}

func (r *Repository) loadPackset(ctx context.Context, s *RemoteState) (model.Packset, error) {
	b, e := r.verified(ctx, s.Head.Packset.Object, model.MaxPackset)
	if e != nil {
		return model.Packset{}, e
	}
	var packset model.Packset
	if e = canonical.UnmarshalForward(b, &packset, model.MaxPackset); e != nil {
		return model.Packset{}, e
	}
	if e = packset.Validate(); e != nil {
		return model.Packset{}, e
	}
	pointer := s.Head.Packset
	if packset.RepositoryID != s.Head.RepositoryID || packset.ObjectFormat != s.Head.ObjectFormat || packset.PacksetID != pointer.PacksetID || packset.Generation != pointer.Generation || !same(packset.TransactionID, pointer.TransactionID) {
		return model.Packset{}, fmt.Errorf("packset pointer mismatch")
	}
	return packset, nil
}

func (r *Repository) installPackset(ctx context.Context, ls local.State, s *RemoteState, progress ProgressSink) (bool, error) {
	ps, e := r.loadPackset(ctx, s)
	if e != nil {
		return false, e
	}
	if e = os.MkdirAll(ls.PackDir, 0755); e != nil {
		return false, e
	}
	packCount := 0
	var pending []model.PackEntry
	var total uint64
	var objects uint64
	for _, l := range ps.Levels {
		for _, p := range l.Packs {
			packCount++
			pp := filepath.Join(ls.PackDir, "pack-"+p.GitPackChecksum+".pack")
			ip := filepath.Join(ls.PackDir, "pack-"+p.GitPackChecksum+".idx")
			packExists, e := pathExists(pp)
			if e != nil {
				return false, e
			}
			indexExists, e := pathExists(ip)
			if e != nil {
				return false, e
			}
			if packExists && indexExists {
				if e = r.verifyPackPair(ctx, p, pp, ip); e == nil {
					continue
				}
			}
			if packExists || indexExists {
				if e = removePackPair(pp, ip); e != nil {
					return false, fmt.Errorf("repair interrupted pack install: %w", e)
				}
			}
			if p.Pack.Size > ^uint64(0)-total || p.Index.Size > ^uint64(0)-total-p.Pack.Size {
				return false, fmt.Errorf("packset download size overflow")
			}
			if p.ObjectCount > ^uint64(0)-objects {
				return false, fmt.Errorf("packset object count overflow")
			}
			total += p.Pack.Size + p.Index.Size
			objects += p.ObjectCount
			pending = append(pending, p)
		}
	}
	var received atomic.Uint64
	var advance func(uint64)
	if progress != nil && total > 0 {
		progress.Update(ProgressEvent{Phase: "Receiving S3 pack data", Total: total, Unit: ProgressUnitBytes})
		advance = func(n uint64) {
			current := received.Add(n)
			progress.Update(ProgressEvent{Phase: "Receiving S3 pack data", Current: current, Total: total, Unit: ProgressUnitBytes})
		}
	}
	for _, p := range pending {
		pp := filepath.Join(ls.PackDir, "pack-"+p.GitPackChecksum+".pack")
		ip := filepath.Join(ls.PackDir, "pack-"+p.GitPackChecksum+".idx")
		if e = r.installPackPair(ctx, ls, p, pp, ip, advance); e != nil {
			return false, e
		}
	}
	if progress != nil && total > 0 {
		progress.Update(ProgressEvent{Phase: "Receiving S3 pack data", Current: total, Total: total, Unit: ProgressUnitBytes, Done: true})
	}
	if progress != nil && objects > 0 {
		progress.Update(ProgressEvent{Phase: "Receiving objects", Current: objects, Total: objects, Unit: ProgressUnitCount, Done: true})
	}
	if packCount >= 4 {
		if e = r.Git.WriteMIDX(ctx); e != nil {
			return false, e
		}
	}
	return len(pending) > 0, nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func removePackPair(packPath, indexPath string) error {
	for _, path := range []string{packPath, indexPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (r *Repository) verifyPackPair(ctx context.Context, entry model.PackEntry, packPath, indexPath string) error {
	pn, packSHA, err := fileDigestLocal(packPath)
	if err != nil || uint64(pn) != entry.Pack.Size || packSHA != entry.Pack.SHA256 {
		return errs.E(errs.IntegrityFailed, "verify-pack", fmt.Errorf("pack digest mismatch"))
	}
	in, indexSHA, err := fileDigestLocal(indexPath)
	if err != nil || uint64(in) != entry.Index.Size || indexSHA != entry.Index.SHA256 {
		return errs.E(errs.IntegrityFailed, "verify-pack", fmt.Errorf("index digest mismatch"))
	}
	if err = verifyTrailer(packPath, entry.GitPackChecksum); err != nil {
		return err
	}
	if err = r.Git.VerifyPack(ctx, indexPath); err != nil {
		return err
	}
	count, err := r.Git.CountPackObjects(ctx, indexPath)
	if err != nil {
		return err
	}
	if count != entry.ObjectCount {
		return fmt.Errorf("pack object count mismatch")
	}
	return nil
}

func (r *Repository) installPackPair(ctx context.Context, ls local.State, entry model.PackEntry, packPath, indexPath string, advance func(uint64)) error {
	stage, err := os.MkdirTemp(ls.Root, "pack-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	base := "pack-" + entry.GitPackChecksum
	stagedPack := filepath.Join(stage, base+".pack")
	stagedIndex := filepath.Join(stage, base+".idx")
	if err = r.downloadVerified(ctx, entry.Pack, stagedPack, advance); err != nil {
		return err
	}
	if err = r.downloadVerified(ctx, entry.Index, stagedIndex, advance); err != nil {
		return err
	}
	if err = r.verifyPackPair(ctx, entry, stagedPack, stagedIndex); err != nil {
		return err
	}
	if err = os.Rename(stagedPack, packPath); err != nil {
		return err
	}
	if err = os.Rename(stagedIndex, indexPath); err != nil {
		_ = os.Remove(packPath)
		return err
	}
	if dir, openErr := os.Open(ls.PackDir); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func fileDigestLocal(path string) (int64, string, error) {
	f, e := os.Open(path)
	if e != nil {
		return 0, "", e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, f)
	return n, hex.EncodeToString(h.Sum(nil)), e
}
func verifyTrailer(path, expected string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	n := int64(len(expected) / 2)
	st, e := f.Stat()
	if e != nil || st.Size() < n {
		return fmt.Errorf("short native pack")
	}
	b := make([]byte, n)
	if _, e = f.ReadAt(b, st.Size()-n); e != nil {
		return e
	}
	if hex.EncodeToString(b) != expected {
		return errs.E(errs.IntegrityFailed, "verify-pack", fmt.Errorf("native checksum mismatch"))
	}
	return nil
}
func (r *Repository) downloadVerified(ctx context.Context, ref model.ObjectRef, path string, advance func(uint64)) error {
	if e := ref.Validate(); e != nil {
		return e
	}
	if ref.Size > uint64(^uint64(0)>>1) {
		return fmt.Errorf("object too large for local file")
	}
	size := int64(ref.Size)
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(e, os.ErrExist) {
		if e = os.Remove(path); e != nil {
			return e
		}
		f, e = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	}
	if e != nil {
		return e
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if e = f.Truncate(size); e != nil {
		return e
	}
	chunk := r.DownloadChunkSize
	if chunk <= 0 {
		chunk = 64 << 20
	}
	workers := r.DownloadConcurrency
	if workers <= 0 {
		workers = 4
	}
	type span struct{ a, b int64 }
	jobs := make(chan span)
	failures := make(chan error, workers)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for x := range jobs {
				writer := io.NewOffsetWriter(f, x.a)
				e := r.copyRange(ctx, ref.Key, x.a, x.b, ref.Size, writer)
				if e != nil {
					select {
					case failures <- e:
					default:
					}
					cancel()
					return
				}
				if advance != nil {
					advance(uint64(x.b - x.a + 1))
				}
			}
		}()
	}
	for a := int64(0); a < size; {
		b := a + chunk - 1
		if b >= size {
			b = size - 1
		}
		select {
		case jobs <- span{a, b}:
			a = b + 1
		case <-ctx.Done():
			a = size
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case e = <-failures:
		return e
	default:
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	n, sum, e := fileDigestLocal(path)
	if e != nil {
		return e
	}
	if uint64(n) != ref.Size || sum != ref.SHA256 {
		return errs.E(errs.IntegrityFailed, "download", fmt.Errorf("digest mismatch for %s", ref.Key))
	}
	ok = true
	return nil
}
func (r *Repository) writeCursor(ctx context.Context, ls local.State, s *RemoteState) error {
	snap := model.Snapshot{RepositoryID: s.Head.RepositoryID, ObjectFormat: s.Head.ObjectFormat, Generation: s.Head.LogicalGeneration, TransactionID: s.Head.TransactionID, Refs: s.Refs}
	b, e := snap.MarshalText()
	if e != nil {
		return e
	}
	if e = ls.Write(model.Cursor{FormatVersion: 1, RepositoryID: s.Head.RepositoryID, ObjectFormat: s.Head.ObjectFormat, LogicalGeneration: s.Head.LogicalGeneration, TransactionID: s.Head.TransactionID, LastPublicationID: s.Head.PublicationID, LastHeadETag: s.ETag}, b, s.ETag); e != nil {
		return e
	}
	hb, e := canonical.Marshal(s.Head)
	if e != nil {
		return e
	}
	if e = ls.WriteHead(hb); e != nil {
		return e
	}
	if r.CacheID != "" {
		return local.WriteRemoteMapping(ctx, r.Git, r.CacheID, s.Head.RepositoryID)
	}
	return nil
}

// ReadUnconditional reads and validates remote state without using the local cache.
func (r *Repository) ReadUnconditional(ctx context.Context) (*RemoteState, error) {
	o, e := r.Store.Get(ctx, ".git/git3/HEAD", "")
	if e != nil {
		return nil, e
	}
	return r.readObject(ctx, o)
}
