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
		return vr.Fetch(ctx, s, tips, true)
	}
	return nil
}
func (r *Repository) verifyDescriptorGraph(ctx context.Context, s *RemoteState) error {
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
	for _, x := range s.Head.Log.Tail {
		if e := check(x); e != nil {
			return e
		}
	}
	p := s.Head.Log.TipPage
	seen := map[string]bool{}
	for p != nil && p.LastGeneration > s.Head.Log.FloorGeneration {
		if seen[p.PageID] {
			return fmt.Errorf("page cycle")
		}
		seen[p.PageID] = true
		b, e := r.verified(ctx, p.Object, model.MaxLogPage)
		if e != nil {
			return e
		}
		var pg model.LogPage
		if e = canonical.UnmarshalForward(b, &pg, model.MaxLogPage); e != nil {
			return e
		}
		for _, x := range pg.Transactions {
			if x.Transaction.Generation > s.Head.Log.FloorGeneration {
				if e = check(x); e != nil {
					return e
				}
			}
		}
		if pg.Previous == nil {
			break
		}
		p = &model.PagePointer{PageID: pg.Previous.PageID, FirstGeneration: pg.Previous.FirstGeneration, LastGeneration: pg.Previous.LastGeneration, Object: pg.Previous.Object}
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
				part, x := r.Store.GetRange(ctx, key, a, z)
				if x != nil {
					ge = x
					break
				}
				h.Write(part)
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

// Maintenance compacts live objects using a geometric fanout policy.
func (r *Repository) Maintenance(ctx context.Context, fanout int) (string, error) {
	if e := r.Git.RequireVersion(ctx, 2, 38); e != nil {
		return "", e
	}
	s, e := r.ReadUnconditional(ctx)
	if e != nil {
		return "", e
	}
	if s.Head.GCBarrier != nil {
		return "", fmt.Errorf("GC barrier active")
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
	pb, e := r.verified(ctx, s.Head.Packset.Object, model.MaxPackset)
	if e != nil {
		return "", e
	}
	var old model.Packset
	if e = canonical.UnmarshalForward(pb, &old, model.MaxPackset); e != nil {
		return "", e
	}
	if e = old.Validate(); e != nil {
		return "", e
	}
	if old.RepositoryID != s.Head.RepositoryID || old.PacksetID != s.Head.Packset.PacksetID || old.Generation != s.Head.Packset.Generation || !same(old.TransactionID, s.Head.Packset.TransactionID) {
		return "", fmt.Errorf("packset pointer mismatch")
	}
	if s.Head.Packset.Generation == s.Head.LogicalGeneration && s.Head.RefSnapshot.Generation == s.Head.LogicalGeneration {
		return s.Head.PublicationID, nil
	}
	targetGen := s.Head.LogicalGeneration
	targetTx := s.Head.TransactionID
	targetRefs := s.Refs
	if r.MaintenanceMaxBytes > 0 {
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
			if used+n > uint64(r.MaintenanceMaxBytes) {
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
					if er = r.downloadVerified(ctx, p.Pack, pp+".part"); er != nil {
						return "", er
					}
					if er = os.Rename(pp+".part", pp); er != nil {
						return "", er
					}
				} else if n, sum, x := fileDigest(pp); x != nil || uint64(n) != p.Pack.Size || sum != p.Pack.SHA256 {
					return "", fmt.Errorf("selected local pack failed verification")
				}
				if _, er := os.Stat(ip); er != nil {
					if er = r.downloadVerified(ctx, p.Index, ip+".part"); er != nil {
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
	if e = h.Validate(); e != nil {
		return "", e
	}
	if e = r.validateBootstrap(ctx, h, s.Transactions, s.Refs); e != nil {
		return "", e
	}
	hb, e := canonical.Marshal(h)
	if e != nil {
		return "", e
	}
	if len(hb) > model.MaxHead {
		return "", fmt.Errorf("prospective HEAD exceeds limit")
	}
	_, e = r.Store.Put(ctx, ".git/git3/HEAD", bytes.NewReader(hb), int64(len(hb)), store.PutOptions{IfMatch: s.ETag})
	if errors.Is(e, store.ErrPrecondition) {
		return "", errs.E(errs.CASConflict, "maintenance", fmt.Errorf("another publisher replaced HEAD"))
	}
	if e != nil {
		observed, re := r.ReadUnconditional(ctx)
		if re != nil {
			return "", errs.E(errs.PublishAmbiguous, "maintenance", e)
		}
		if observed.Head.PublicationID == h.PublicationID || observed.Head.Packset.PacksetID == packID {
			return h.PublicationID, nil
		}
		return "", errs.E(errs.PublishAmbiguous, "maintenance", e)
	}
	return h.PublicationID, e
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
	if e = h.Validate(); e != nil {
		return "", e
	}
	b, e := canonical.Marshal(h)
	if e != nil {
		return "", e
	}
	_, e = r.Store.Put(ctx, ".git/git3/HEAD", bytes.NewReader(b), int64(len(b)), store.PutOptions{IfMatch: s.ETag})
	if errors.Is(e, store.ErrPrecondition) {
		return "", errs.E(errs.CASConflict, "set-head", fmt.Errorf("another publisher replaced HEAD"))
	}
	if e != nil {
		observed, re := r.ReadUnconditional(ctx)
		if re != nil {
			return "", errs.E(errs.PublishAmbiguous, "set-head", e)
		}
		if observed.Head.PublicationID == h.PublicationID {
			return h.PublicationID, nil
		}
		return "", errs.E(errs.PublishAmbiguous, "set-head", e)
	}
	return h.PublicationID, e
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
	live := map[string]bool{".git/git3/HEAD": true, s.Head.RefSnapshot.Object.Key: true, s.Head.Packset.Object.Key: true}
	pb, e := r.verified(ctx, s.Head.Packset.Object, model.MaxPackset)
	if e != nil {
		return nil, e
	}
	var ps model.Packset
	if e = canonical.UnmarshalForward(pb, &ps, model.MaxPackset); e != nil {
		return nil, e
	}
	for _, l := range ps.Levels {
		for _, p := range l.Packs {
			live[p.Pack.Key] = true
			live[p.Index.Key] = true
		}
	}
	for _, e := range s.Head.Log.Tail {
		live[e.Descriptor.Key] = true
		if e.Transaction.ObjectData != nil {
			live[e.Transaction.ObjectData.Object.Key] = true
		}
	}
	p := s.Head.Log.TipPage
	seen := map[string]bool{}
	for p != nil && p.LastGeneration > s.Head.Log.FloorGeneration {
		if seen[p.PageID] {
			return nil, fmt.Errorf("page cycle")
		}
		seen[p.PageID] = true
		live[p.Object.Key] = true
		b, e := r.verified(ctx, p.Object, model.MaxLogPage)
		if e != nil {
			return nil, e
		}
		var pg model.LogPage
		if e = canonical.UnmarshalForward(b, &pg, model.MaxLogPage); e != nil {
			return nil, e
		}
		for _, x := range pg.Transactions {
			if x.Transaction.Generation > s.Head.Log.FloorGeneration {
				live[x.Descriptor.Key] = true
				if x.Transaction.ObjectData != nil {
					live[x.Transaction.ObjectData.Object.Key] = true
				}
			}
		}
		if pg.Previous == nil {
			break
		}
		p = &model.PagePointer{PageID: pg.Previous.PageID, FirstGeneration: pg.Previous.FirstGeneration, LastGeneration: pg.Previous.LastGeneration, Object: pg.Previous.Object}
	}
	if s.Head.GCBarrier != nil {
		live[s.Head.GCBarrier.Plan.Key] = true
	}
	return live, nil
}

// GCDryRun discovers unreachable objects without changing remote state.
func (r *Repository) GCDryRun(ctx context.Context, cutoff time.Time) (GCReport, error) {
	s, e := r.Read(ctx)
	if e != nil {
		return GCReport{}, e
	}
	live, e := r.liveKeys(ctx, s)
	if e != nil {
		return GCReport{}, e
	}
	all, e := r.Store.List(ctx, ".git/git3/")
	if e != nil {
		return GCReport{}, e
	}
	rep := GCReport{RepositoryID: s.Head.RepositoryID, PublicationID: s.Head.PublicationID, ETag: s.ETag, Cutoff: cutoff.UTC().Format(time.RFC3339)}
	for _, x := range all {
		if e := locator.ValidateManagedKey(x.Key); e != nil {
			return GCReport{}, e
		}
		if live[x.Key] || x.Key == ".git/git3/HEAD" || strings.HasPrefix(x.Key, ".git/git3/probes/") || strings.HasPrefix(x.Key, ".git/git3/lfs/") || !x.LastModified.Before(cutoff) {
			continue
		}
		c := model.GCCandidate{Key: x.Key, Size: uint64(x.Size), ETag: x.ETag, LastModified: x.LastModified.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z"), Category: gcCategory(x.Key)}
		rep.Candidates = append(rep.Candidates, c)
		rep.TotalBytes += uint64(x.Size)
	}
	sort.Slice(rep.Candidates, func(i, j int) bool { return rep.Candidates[i].Key < rep.Candidates[j].Key })
	return rep, nil
}
func gcCategory(k string) string {
	p := strings.TrimPrefix(k, ".git/git3/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}

// GCExecute publishes a barrier and conditionally deletes planned candidates.
func (r *Repository) GCExecute(ctx context.Context, cutoff time.Time) (string, error) {
	rep, e := r.GCDryRun(ctx, cutoff)
	if e != nil {
		return "", e
	}
	s := r.Pinned
	if e = r.ensureWritePolicy(s); e != nil {
		return "", e
	}
	if s.Head.GCBarrier != nil {
		return "", fmt.Errorf("GC barrier active")
	}
	if len(rep.Candidates) > model.MaxRefs {
		return "", fmt.Errorf("GC candidate set exceeds %d; execute separately barriered plans", model.MaxRefs)
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
	if e = h.Validate(); e != nil {
		return "", e
	}
	hb, e := canonical.Marshal(h)
	if e != nil {
		return "", e
	}
	_, e = r.Store.Put(ctx, ".git/git3/HEAD", bytes.NewReader(hb), int64(len(hb)), store.PutOptions{IfMatch: s.ETag})
	if e != nil {
		return "", e
	}
	s, e = r.Read(ctx)
	if e != nil {
		return "", e
	}
	if s.Head.GCBarrier == nil || s.Head.GCBarrier.PlanID != id {
		return "", fmt.Errorf("GC barrier changed")
	}
	live, e := r.liveKeys(ctx, s)
	if e != nil {
		return "", e
	}
	for _, c := range plan.Candidates {
		if live[c.Key] {
			return "", fmt.Errorf("candidate became live: %s", c.Key)
		}
		md, e := r.Store.Head(ctx, c.Key)
		if e != nil {
			return "", e
		}
		if md.ETag != c.ETag || uint64(md.Size) != c.Size || md.LastModified.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z") != c.LastModified {
			return "", fmt.Errorf("candidate changed: %s", c.Key)
		}
	}
	for _, c := range plan.Candidates {
		if e = r.Store.Delete(ctx, c.Key, store.DeleteOptions{IfMatch: c.ETag}); e != nil {
			return "", e
		}
	}
	s, e = r.Read(ctx)
	if e != nil {
		return "", e
	}
	h = s.Head
	if h.GCBarrier == nil || h.GCBarrier.PlanID != id {
		return "", fmt.Errorf("GC barrier changed")
	}
	h.GCBarrier = nil
	h.ManifestRevision++
	h.PublicationID = uuid.NewString()
	if e = h.Validate(); e != nil {
		return "", e
	}
	hb, e = canonical.Marshal(h)
	if e != nil {
		return "", e
	}
	_, e = r.Store.Put(ctx, ".git/git3/HEAD", bytes.NewReader(hb), int64(len(hb)), store.PutOptions{IfMatch: s.ETag})
	return h.PublicationID, e
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
	if s.Head.GCBarrier == nil || s.Head.GCBarrier.PlanID != id {
		return "", fmt.Errorf("GC plan is not active")
	}
	h := s.Head
	h.GCBarrier = nil
	h.ManifestRevision++
	h.PublicationID = uuid.NewString()
	if e = h.Validate(); e != nil {
		return "", e
	}
	b, e := canonical.Marshal(h)
	if e != nil {
		return "", e
	}
	_, e = r.Store.Put(ctx, ".git/git3/HEAD", bytes.NewReader(b), int64(len(b)), store.PutOptions{IfMatch: s.ETag})
	return h.PublicationID, e
}

// GCResume continues deletion for the identified garbage-collection plan.
func (r *Repository) GCResume(ctx context.Context, id string) (string, error) {
	s, e := r.Read(ctx)
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
	live, e := r.liveKeys(ctx, s)
	if e != nil {
		return "", e
	}
	for _, c := range p.Candidates {
		if strings.HasPrefix(c.Key, ".git/git3/lfs/") {
			return "", fmt.Errorf("GC plan contains protected LFS object: %s", c.Key)
		}
		if live[c.Key] {
			return "", fmt.Errorf("candidate became live: %s", c.Key)
		}
		m, e := r.Store.Head(ctx, c.Key)
		if errors.Is(e, store.ErrNotFound) {
			continue
		}
		if e != nil {
			return "", e
		}
		if m.ETag != c.ETag || uint64(m.Size) != c.Size || m.LastModified.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z") != c.LastModified {
			return "", fmt.Errorf("candidate changed: %s", c.Key)
		}
	}
	for _, c := range p.Candidates {
		e = r.Store.Delete(ctx, c.Key, store.DeleteOptions{IfMatch: c.ETag})
		if errors.Is(e, store.ErrNotFound) {
			continue
		}
		if e != nil {
			return "", e
		}
	}
	return r.GCAbort(ctx, id)
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
