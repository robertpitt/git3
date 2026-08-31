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
	Pinned                   *RemoteState
	StoragePolicy            model.StoragePolicy
	DryRun                   bool
	DownloadChunkSize        int64
	DownloadConcurrency      int
	CacheID                  string
	InstalledKeeps           []string
	Warnings                 []string
	CompactionFanout         int
	CompactAfterTransactions int
	MaintenanceMaxBytes      int64
	CompactAfterBytes        uint64
}

// RemoteState is a validated materialized view of a remote repository.
type RemoteState struct {
	Head         model.Head
	ETag         string
	Refs         map[string]string
	Transactions []model.Transaction
	Cached       bool
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
			return ref, fmt.Errorf("immutable collision at %s", key)
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
		return nil, fmt.Errorf("integrity mismatch for %s", ref.Key)
	}
	return o.Body, nil
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
	s := &RemoteState{Head: h, ETag: c.LastHeadETag, Refs: snap.Refs, Cached: true}
	r.Pinned = s
	return s, true, nil
}
func (r *Repository) readObject(ctx context.Context, o store.Object) (*RemoteState, error) {
	var e error
	if len(o.Body) > model.MaxHead {
		return nil, fmt.Errorf("HEAD too large")
	}
	var h model.Head
	if e = canonical.UnmarshalForward(o.Body, &h, model.MaxHead); e != nil {
		return nil, e
	}
	if e = h.Validate(); e != nil {
		return nil, e
	}
	txs, e := r.collectTransactions(ctx, h)
	if e != nil {
		return nil, e
	}
	refs, e := r.reconstruct(ctx, h, txs)
	if e != nil {
		return nil, e
	}
	s := &RemoteState{Head: h, ETag: o.ETag, Refs: refs, Transactions: txs}
	r.Pinned = s
	return s, nil
}
func (r *Repository) collectTransactions(ctx context.Context, h model.Head) ([]model.Transaction, error) {
	var pages [][]model.Envelope
	seen := map[string]bool{}
	p := h.Log.TipPage
	for p != nil && p.LastGeneration > h.Log.FloorGeneration {
		if _, e := uuid.Parse(p.PageID); e != nil || p.FirstGeneration > p.LastGeneration {
			return nil, fmt.Errorf("invalid log page pointer")
		}
		if seen[p.PageID] {
			return nil, fmt.Errorf("log page cycle")
		}
		seen[p.PageID] = true
		b, e := r.verified(ctx, p.Object, model.MaxLogPage)
		if e != nil {
			return nil, e
		}
		var page model.LogPage
		if e = canonical.UnmarshalForward(b, &page, model.MaxLogPage); e != nil {
			return nil, e
		}
		if page.FormatVersion != 1 || page.RepositoryID != h.RepositoryID || page.PageID != p.PageID || len(page.Transactions) == 0 || len(page.Transactions) > 32 {
			return nil, fmt.Errorf("invalid log page")
		}
		if page.FirstGeneration != page.Transactions[0].Transaction.Generation || page.LastGeneration != page.Transactions[len(page.Transactions)-1].Transaction.Generation {
			return nil, fmt.Errorf("log page bounds mismatch")
		}
		first := page.Transactions[0].Transaction
		if page.BaseGeneration != first.ParentGeneration || !same(page.BaseTransactionID, first.ParentTransactionID) {
			return nil, fmt.Errorf("log page base mismatch")
		}
		if page.Previous != nil && (page.Previous.LastGeneration != page.BaseGeneration || page.Previous.FirstGeneration > page.Previous.LastGeneration) {
			return nil, fmt.Errorf("log page previous mismatch")
		}
		pages = append(pages, page.Transactions)
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
	gen := h.Log.FloorGeneration
	pid := h.Log.FloorTransactionID
	for _, e := range envs {
		t := e.Transaction
		if t.Generation <= h.Log.FloorGeneration {
			continue
		}
		if er := t.Validate(); er != nil {
			return nil, er
		}
		if t.RepositoryID != h.RepositoryID || t.ObjectFormat != h.ObjectFormat || t.Generation != gen+1 || !same(pid, t.ParentTransactionID) {
			return nil, fmt.Errorf("transaction chain gap at %d", t.Generation)
		}
		if er := e.Descriptor.Validate(); er != nil {
			return nil, er
		}
		out = append(out, t)
		gen = t.Generation
		x := t.TransactionID
		pid = &x
	}
	if gen != h.LogicalGeneration || !same(pid, h.TransactionID) {
		return nil, fmt.Errorf("transaction chain does not reach HEAD")
	}
	return out, nil
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
func (r *Repository) Push(ctx context.Context, cmds []PushCommand, atomic bool) ([]PushResult, error) {
	if e := r.Git.RequireVersion(ctx, 2, 38); e != nil {
		return nil, e
	}
	state := r.Pinned
	if state == nil {
		var e error
		state, e = r.Read(ctx)
		if errors.Is(e, store.ErrNotFound) {
			return r.initialize(ctx, cmds, atomic)
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
		return nil, fmt.Errorf("object format mismatch: local %s remote %s", format, state.Head.ObjectFormat)
	}
	if r.StoragePolicy.ServerSideEncryption != "" && !policyEqual(r.StoragePolicy, state.Head.StoragePolicy) {
		return nil, fmt.Errorf("local encryption settings conflict with repository storage policy")
	}
	updates, res := r.validatePush(ctx, state, cmds)
	if rejectAtomicBatch(res, atomic) {
		return res, nil
	}
	if len(updates) == 0 {
		return res, nil
	}
	if r.DryRun {
		return res, nil
	}
	tx, e := r.makeTransaction(ctx, state, updates)
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
	if e = candidate.Validate(); e != nil {
		return res, e
	}
	b, e := canonical.Marshal(candidate)
	if e != nil || len(b) > model.MaxHead {
		return res, fmt.Errorf("prospective HEAD invalid or too large: %w", e)
	}
	_, e = r.Store.Put(ctx, ".git/git3/HEAD", bytes.NewReader(b), int64(len(b)), store.PutOptions{IfMatch: state.ETag})
	if errors.Is(e, store.ErrPrecondition) {
		return res, errs.E(errs.CASConflict, "publish", fmt.Errorf("another writer replaced HEAD"))
	}
	if e != nil {
		observed, readErr := r.ReadUnconditional(ctx)
		if readErr != nil {
			return res, errs.E(errs.PublishAmbiguous, "publish", e)
		}
		for _, x := range observed.Transactions {
			if x.Generation == tx.Generation {
				a, _ := canonical.Marshal(x)
				b, _ := canonical.Marshal(tx)
				if bytes.Equal(a, b) {
					r.Pinned = nil
					return res, nil
				}
				return res, errs.E(errs.CASConflict, "publish", fmt.Errorf("generation %d belongs to another transaction", tx.Generation))
			}
		}
		if observed.Head.LogicalGeneration <= state.Head.LogicalGeneration {
			return res, e
		}
		return res, errs.E(errs.PublishAmbiguous, "publish", e)
	}
	r.Pinned = nil
	return res, nil
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

// ValidateWritePolicy checks that writes use the published repository encryption policy.
// An absent repository has no published policy, so its local policy remains authoritative.
func (r *Repository) ValidateWritePolicy(ctx context.Context) error {
	s, err := r.Read(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.ensureWritePolicy(s)
}
func (r *Repository) validatePush(ctx context.Context, s *RemoteState, cmds []PushCommand) ([]model.Update, []PushResult) {
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
		if !model.ValidOID(*c.NewOID, s.Head.ObjectFormat) || !r.Git.HasObject(ctx, *c.NewOID) {
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
			if strings.HasPrefix(c.Dst, "refs/heads/") && r.Git.IsAncestor(ctx, old, *c.NewOID) {
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
	return us, res
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
	t.tail = append(t.tail, p[:n]...)
	if len(t.tail) > t.want {
		t.tail = append([]byte(nil), t.tail[len(t.tail)-t.want:]...)
	}
	return n, e
}
func (r *Repository) makeTransaction(ctx context.Context, s *RemoteState, updates []model.Update) (model.Transaction, error) {
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
	for _, oid := range s.Refs {
		if r.Git.HasObject(ctx, oid) {
			neg = append(neg, oid)
		}
	}
	pr, e := r.Git.PackObjects(ctx, pos, neg, true)
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

func (r *Repository) initialize(ctx context.Context, cmds []PushCommand, atomic bool) ([]PushResult, error) {
	format, e := r.Git.ObjectFormat(ctx)
	if e != nil {
		return nil, e
	}
	repoID := uuid.NewString()
	zero := &RemoteState{Head: model.Head{RepositoryID: repoID, ObjectFormat: format, LogicalGeneration: 0, TransactionID: nil}, Refs: map[string]string{}}
	updates, res := r.validatePush(ctx, zero, cmds)
	if rejectAtomicBatch(res, atomic) {
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
	if r.DryRun {
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
	tx, e := r.makeTransaction(ctx, zero, updates)
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
	if e = head.Validate(); e != nil {
		return res, e
	}
	prospectiveRefs := map[string]string{}
	if e = model.Apply(prospectiveRefs, []model.Transaction{tx}); e != nil {
		return res, e
	}
	if e = r.validateBootstrap(ctx, head, []model.Transaction{tx}, prospectiveRefs); e != nil {
		return res, e
	}
	hb, e := canonical.Marshal(head)
	if e != nil {
		return res, e
	}
	_, e = r.Store.Put(ctx, ".git/git3/HEAD", bytes.NewReader(hb), int64(len(hb)), store.PutOptions{IfNoneMatch: true})
	if errors.Is(e, store.ErrPrecondition) {
		return res, errs.E(errs.CASConflict, "initialize", fmt.Errorf("repository was initialized concurrently"))
	}
	if e != nil {
		observed, re := r.ReadUnconditional(ctx)
		if re != nil {
			return res, errs.E(errs.PublishAmbiguous, "initialize", e)
		}
		for _, x := range observed.Transactions {
			if x.Generation == 1 && x.TransactionID == tx.TransactionID {
				return res, nil
			}
		}
		return res, errs.E(errs.CASConflict, "initialize", fmt.Errorf("another initialization was published"))
	}
	return res, e
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
	return vr.Fetch(ctx, &RemoteState{Head: h, Refs: refs, Transactions: txs}, tips, true)
}

// Fetch installs requested objects and optionally verifies repository connectivity.
func (r *Repository) Fetch(ctx context.Context, s *RemoteState, requested []string, connectivity bool) error {
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
	if c, e := ls.ReadCursor(); e == nil {
		if c.LogicalGeneration > s.Head.LogicalGeneration || (c.LogicalGeneration == s.Head.LogicalGeneration && !same(c.TransactionID, s.Head.TransactionID)) {
			r.Warnings = append(r.Warnings, "remote logical history is lower or divergent; retaining local objects and bootstrapping")
		}
	}
	if c, e := ls.ReadCursor(); e == nil && c.LogicalGeneration == s.Head.LogicalGeneration && same(c.TransactionID, s.Head.TransactionID) {
		ok := true
		for _, oid := range requested {
			if !r.Git.HasObject(ctx, oid) {
				ok = false
			}
		}
		if ok {
			if c.LastHeadETag != s.ETag || c.LastPublicationID != s.Head.PublicationID {
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
	if c, e := ls.ReadCursor(); e == nil && c.LogicalGeneration >= s.Head.Log.FloorGeneration && c.LogicalGeneration < s.Head.LogicalGeneration && cursorOnChain(s, c) {
		ok := true
		for _, t := range s.Transactions {
			if t.Generation > c.LogicalGeneration && t.ObjectData != nil {
				if e = r.installWAL(ctx, *t.ObjectData); e != nil {
					ok = false
					break
				}
			}
		}
		if ok && r.Git.VerifyConnectivity(ctx, s.Refs) == nil {
			return r.writeCursor(ctx, ls, s)
		}
	}
	if e = r.installPackset(ctx, ls, s); e != nil {
		return e
	}
	for _, t := range s.Transactions {
		if t.Generation > s.Head.Packset.Generation && t.ObjectData != nil {
			if e = r.installWAL(ctx, *t.ObjectData); e != nil {
				return e
			}
		}
	}
	if e = r.Git.VerifyConnectivity(ctx, s.Refs); e != nil {
		return e
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
func (r *Repository) installWAL(ctx context.Context, d model.ObjectData) error {
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
			part, e := r.Store.GetRange(ctx, d.Object.Key, a, b)
			if e != nil {
				pw.CloseWithError(e)
				return
			}
			if int64(len(part)) != b-a+1 {
				pw.CloseWithError(fmt.Errorf("short WAL range"))
				return
			}
			if _, e = h.Write(part); e != nil {
				pw.CloseWithError(e)
				return
			}
			if _, e = pw.Write(part); e != nil {
				return
			}
			total += uint64(len(part))
			a = b + 1
		}
		if total != d.Object.Size || hex.EncodeToString(h.Sum(nil)) != d.Object.SHA256 {
			pw.CloseWithError(fmt.Errorf("WAL integrity mismatch"))
			return
		}
		pw.Close()
	}()
	out, e := r.Git.IndexPack(ctx, pr, true, "git3-fetch")
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
					r.InstalledKeeps = append(r.InstalledKeeps, p)
				}
			}
		}
	}
	return nil
}
func (r *Repository) installPackset(ctx context.Context, ls local.State, s *RemoteState) error {
	b, e := r.verified(ctx, s.Head.Packset.Object, model.MaxPackset)
	if e != nil {
		return e
	}
	var ps model.Packset
	if e = canonical.UnmarshalForward(b, &ps, model.MaxPackset); e != nil {
		return e
	}
	if e = ps.Validate(); e != nil {
		return e
	}
	if ps.RepositoryID != s.Head.RepositoryID || ps.Generation != s.Head.Packset.Generation || !same(ps.TransactionID, s.Head.Packset.TransactionID) {
		return fmt.Errorf("packset pointer mismatch")
	}
	if e = os.MkdirAll(ls.PackDir, 0755); e != nil {
		return e
	}
	packCount := 0
	for _, l := range ps.Levels {
		for _, p := range l.Packs {
			packCount++
			pp := filepath.Join(ls.PackDir, "pack-"+p.GitPackChecksum+".pack")
			ip := filepath.Join(ls.PackDir, "pack-"+p.GitPackChecksum+".idx")
			packExists, e := pathExists(pp)
			if e != nil {
				return e
			}
			indexExists, e := pathExists(ip)
			if e != nil {
				return e
			}
			if packExists && indexExists {
				if e = r.verifyPackPair(ctx, p, pp, ip); e == nil {
					continue
				}
			}
			if packExists || indexExists {
				if e = removePackPair(pp, ip); e != nil {
					return fmt.Errorf("repair interrupted pack install: %w", e)
				}
			}
			if e = r.installPackPair(ctx, ls, p, pp, ip); e != nil {
				return e
			}
		}
	}
	if packCount >= 4 {
		if e = r.Git.WriteMIDX(ctx); e != nil {
			return e
		}
	}
	return nil
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
		return fmt.Errorf("pack integrity mismatch")
	}
	in, indexSHA, err := fileDigestLocal(indexPath)
	if err != nil || uint64(in) != entry.Index.Size || indexSHA != entry.Index.SHA256 {
		return fmt.Errorf("index integrity mismatch")
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

func (r *Repository) installPackPair(ctx context.Context, ls local.State, entry model.PackEntry, packPath, indexPath string) error {
	stage, err := os.MkdirTemp(ls.Root, "pack-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	base := "pack-" + entry.GitPackChecksum
	stagedPack := filepath.Join(stage, base+".pack")
	stagedIndex := filepath.Join(stage, base+".idx")
	if err = r.downloadVerified(ctx, entry.Pack, stagedPack); err != nil {
		return err
	}
	if err = r.downloadVerified(ctx, entry.Index, stagedIndex); err != nil {
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
		return fmt.Errorf("native pack checksum mismatch")
	}
	return nil
}
func (r *Repository) downloadVerified(ctx context.Context, ref model.ObjectRef, path string) error {
	if e := ref.Validate(); e != nil {
		return e
	}
	md, e := r.Store.Head(ctx, ref.Key)
	if e != nil {
		return e
	}
	if md.Size < 0 || uint64(md.Size) != ref.Size {
		return fmt.Errorf("remote size mismatch for %s", ref.Key)
	}
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
	if e = f.Truncate(md.Size); e != nil {
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
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for x := range jobs {
				b, e := r.Store.GetRange(ctx, ref.Key, x.a, x.b)
				if e == nil && int64(len(b)) != x.b-x.a+1 {
					e = fmt.Errorf("short range read")
				}
				if e == nil {
					_, e = f.WriteAt(b, x.a)
				}
				if e != nil {
					select {
					case errs <- e:
					default:
					}
					cancel()
					return
				}
			}
		}()
	}
	for a := int64(0); a < md.Size; {
		b := a + chunk - 1
		if b >= md.Size {
			b = md.Size - 1
		}
		select {
		case jobs <- span{a, b}:
			a = b + 1
		case <-ctx.Done():
			a = md.Size
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case e = <-errs:
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
		return fmt.Errorf("download integrity mismatch for %s", ref.Key)
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
