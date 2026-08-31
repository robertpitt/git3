package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertpitt/git3/internal/canonical"
	gitx "github.com/robertpitt/git3/internal/git"
	"github.com/robertpitt/git3/internal/local"
	"github.com/robertpitt/git3/internal/model"
	"github.com/robertpitt/git3/internal/store"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v: %s", args, e, b)
	}
	return strings.TrimSpace(string(b))
}
func TestPushFetchAndIncrementalFetch(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	src := t.TempDir()
	run(t, src, "init", "-q", "-b", "main")
	run(t, src, "config", "user.email", "test@example.com")
	run(t, src, "config", "user.name", "Test")
	if e := os.WriteFile(filepath.Join(src, "hello"), []byte("one\n"), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, src, "add", "hello")
	run(t, src, "commit", "-qm", "one")
	one := run(t, src, "rev-parse", "HEAD")
	mem := store.NewMemory()
	writer := &Repository{Store: mem, Git: gitx.Git{Dir: src}, Version: "test"}
	res, e := writer.Push(ctx, []PushCommand{{Dst: "refs/heads/main", NewOID: &one}}, true)
	if e != nil {
		t.Fatal(e)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("results %#v", res)
	}
	state, e := writer.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if state.Refs["refs/heads/main"] != one {
		t.Fatal("remote ref mismatch")
	}
	dst := t.TempDir()
	run(t, dst, "init", "--bare", "-q")
	reader := &Repository{Store: mem, Git: gitx.Git{Dir: dst}, Version: "test", CacheID: "test-remote"}
	s, e := reader.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = reader.Fetch(ctx, s, []string{one}, true); e != nil {
		t.Fatal(e)
	}
	if !reader.Git.HasObject(ctx, one) {
		t.Fatal("first object not installed")
	}
	mem.Requests = nil
	noop := &Repository{Store: mem, Git: gitx.Git{Dir: dst}, Version: "test", CacheID: "test-remote"}
	cached, e := noop.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if !cached.Cached {
		t.Fatal("expected conditional cache hit")
	}
	if e = noop.Fetch(ctx, cached, []string{one}, true); e != nil {
		t.Fatal(e)
	}
	if len(mem.Requests) != 1 || !strings.HasPrefix(mem.Requests[0], "GET .git/git3/HEAD") {
		t.Fatalf("no-op trace: %#v", mem.Requests)
	}
	if e = os.WriteFile(filepath.Join(src, "hello"), []byte("two\n"), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, src, "commit", "-qam", "two")
	two := run(t, src, "rev-parse", "HEAD")
	writer.Pinned = nil
	if _, e = writer.Push(ctx, []PushCommand{{Dst: "refs/heads/main", NewOID: &two}}, true); e != nil {
		t.Fatal(e)
	}
	s, e = reader.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = reader.Fetch(ctx, s, []string{two}, true); e != nil {
		t.Fatal(e)
	}
	if !reader.Git.HasObject(ctx, two) {
		t.Fatal("incremental object not installed")
	}
	writer.Pinned = nil
	budgetState, e := writer.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	writer.MaintenanceMaxBytes = int64(budgetState.Transactions[0].ObjectData.Object.Size)
	writer.Pinned = nil
	if _, e = writer.Maintenance(ctx, 4); e != nil {
		t.Fatal(e)
	}
	mid, e := writer.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if mid.Head.Packset.Generation != 1 || mid.Head.LogicalGeneration != 2 {
		t.Fatalf("budgeted maintenance selected generation %d", mid.Head.Packset.Generation)
	}
	writer.MaintenanceMaxBytes = 0
	writer.Pinned = nil
	if _, e = writer.Maintenance(ctx, 4); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(src, "hello"), []byte("three\n"), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, src, "commit", "-qam", "three")
	three := run(t, src, "rev-parse", "HEAD")
	writer.Pinned = nil
	if _, e = writer.Push(ctx, []PushCommand{{Dst: "refs/heads/main", NewOID: &three}}, true); e != nil {
		t.Fatal(e)
	}
	writer.Pinned = nil
	if _, e = writer.Maintenance(ctx, 2); e != nil {
		t.Fatal(e)
	}
	cold := t.TempDir()
	run(t, cold, "init", "--bare", "-q")
	coldReader := &Repository{Store: mem, Git: gitx.Git{Dir: cold}, Version: "test", DownloadChunkSize: 32, DownloadConcurrency: 2}
	s, e = coldReader.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	packsetBytes, e := mem.Get(ctx, s.Head.Packset.Object.Key, "")
	if e != nil {
		t.Fatal(e)
	}
	var packset model.Packset
	if e = canonical.UnmarshalForward(packsetBytes.Body, &packset, model.MaxPackset); e != nil {
		t.Fatal(e)
	}
	if len(packset.Levels) == 0 || len(packset.Levels[0].Packs) == 0 {
		t.Fatal("maintenance did not publish a pack")
	}
	interrupted := packset.Levels[0].Packs[0]
	packDir, e := coldReader.Git.PackDir(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(packDir, 0755); e != nil {
		t.Fatal(e)
	}
	interruptedPath := filepath.Join(packDir, "pack-"+interrupted.GitPackChecksum+".pack")
	if e = os.WriteFile(interruptedPath, []byte("interrupted install"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = coldReader.Fetch(ctx, s, []string{three}, true); e != nil {
		t.Fatalf("fetch did not recover an interrupted pack pair install: %v", e)
	}
	if !coldReader.Git.HasObject(ctx, three) {
		t.Fatal("compacted bootstrap object not installed")
	}
	for _, q := range mem.Requests {
		if strings.HasPrefix(q, "LIST ") || strings.HasPrefix(q, "DELETE ") {
			t.Fatalf("normal operation issued forbidden request: %s", q)
		}
	}
	rep, e := writer.GCDryRun(ctx, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	if len(rep.Candidates) == 0 {
		t.Fatal("expected compacted orphans")
	}
	lfsKey := ".git/git3/lfs/objects/aa/bb/" + strings.Repeat("a", 64)
	mem.Set(lfsKey, []byte("protected LFS payload"))
	writer.Pinned = nil
	rep, e = writer.GCDryRun(ctx, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	for _, candidate := range rep.Candidates {
		if candidate.Key == lfsKey {
			t.Fatal("LFS object was classified as garbage")
		}
	}
	if _, e = writer.GCExecute(ctx, time.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = mem.Head(ctx, lfsKey); e != nil {
		t.Fatalf("GC removed protected LFS object: %v", e)
	}
	if e = writer.Fsck(ctx, false); e != nil {
		t.Fatal(e)
	}
	if e = writer.Fsck(ctx, true); e != nil {
		t.Fatal(e)
	}
}

func TestPushDoesNotTrustTamperedCachedRefs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "f")
	run(t, dir, "commit", "-qm", "one")
	one := run(t, dir, "rev-parse", "HEAD")

	mem := store.NewMemory()
	seed := &Repository{Store: mem, Git: gitx.Git{Dir: dir}, Version: "test"}
	if _, err := seed.Push(ctx, []PushCommand{{Dst: "refs/heads/main", NewOID: &one}}, true); err != nil {
		t.Fatal(err)
	}

	cached := &Repository{Store: mem, Git: gitx.Git{Dir: dir}, Version: "test", CacheID: "tampered-remote"}
	state, err := cached.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = cached.Fetch(ctx, state, []string{one}, true); err != nil {
		t.Fatal(err)
	}
	ls, err := local.Resolve(ctx, cached.Git, state.Head.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := ls.ReadCursor()
	if err != nil {
		t.Fatal(err)
	}
	tampered, err := (model.Snapshot{
		RepositoryID:  state.Head.RepositoryID,
		ObjectFormat:  state.Head.ObjectFormat,
		Generation:    state.Head.LogicalGeneration,
		TransactionID: state.Head.TransactionID,
		Refs:          map[string]string{},
	}).MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if err = ls.Write(cursor, tampered, cursor.LastHeadETag); err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(filepath.Join(dir, "f"), []byte("two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "commit", "-qam", "two")
	two := run(t, dir, "rev-parse", "HEAD")
	writer := &Repository{Store: mem, Git: gitx.Git{Dir: dir}, Version: "test", CacheID: "tampered-remote"}
	advertised, err := writer.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !advertised.Cached {
		t.Fatal("expected the tampered cache to exercise the conditional-read path")
	}
	if _, err = writer.Push(ctx, []PushCommand{{Dst: "refs/heads/main", NewOID: &two}}, true); err != nil {
		t.Fatalf("push from a cache hit should use verified remote refs: %v", err)
	}
	verified, err := writer.ReadUnconditional(ctx)
	if err != nil {
		t.Fatalf("push published an unreadable transaction chain: %v", err)
	}
	if got := verified.Refs["refs/heads/main"]; got != two {
		t.Fatalf("remote main = %s, want %s", got, two)
	}
}

func TestAtomicInitialPushRejectsEntireInvalidBatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "f")
	run(t, dir, "commit", "-qm", "one")
	one := run(t, dir, "rev-parse", "HEAD")

	mem := store.NewMemory()
	repo := &Repository{Store: mem, Git: gitx.Git{Dir: dir}, Version: "test"}
	results, err := repo.Push(ctx, []PushCommand{
		{Dst: "refs/heads/main", NewOID: &one},
		{Dst: "refs/heads/.invalid", NewOID: &one},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.OK {
			t.Fatalf("atomic initialization partially accepted %#v", results)
		}
	}
	if _, err = mem.Get(ctx, ".git/git3/HEAD", ""); err != store.ErrNotFound {
		t.Fatalf("atomic initialization published HEAD: %v", err)
	}
}
