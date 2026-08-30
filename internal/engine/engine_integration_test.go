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
	t.Setenv("LC_ALL", "C")
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
	pushProgress := &recordingProgress{}
	res, e := writer.Push(ctx, nil, []PushCommand{{Dst: "refs/heads/main", NewOID: &one}}, PushOptions{Atomic: true, Progress: pushProgress})
	if e != nil {
		t.Fatal(e)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("results %#v", res)
	}
	for _, text := range []string{"Enumerating objects:", "Counting objects:", "Writing objects:"} {
		if !strings.Contains(pushProgress.output(), text) {
			t.Fatalf("native push progress was not relayed: %q", pushProgress.output())
		}
	}
	for _, phase := range []string{"Finalizing S3 upload", "Verifying uploaded objects", "Publishing refs"} {
		if !pushProgress.completed(phase) {
			t.Fatalf("push did not complete progress phase %q", phase)
		}
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
	fetchProgress := &recordingProgress{}
	if _, e = reader.Fetch(ctx, advertisement(s), []string{one}, FetchOptions{Progress: fetchProgress}); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(fetchProgress.output(), "Receiving objects:") {
		t.Fatalf("native fetch progress was not relayed: %q", fetchProgress.output())
	}
	for _, phase := range []string{"Applying transaction packs", "Verifying object connectivity"} {
		if !fetchProgress.completed(phase) {
			t.Fatalf("fetch did not complete progress phase %q", phase)
		}
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
	noopProgress := &recordingProgress{}
	if _, e = noop.Fetch(ctx, advertisement(cached), []string{one}, FetchOptions{Progress: noopProgress}); e != nil {
		t.Fatal(e)
	}
	if !noopProgress.empty() {
		t.Fatal("no-op fetch emitted progress")
	}
	if len(mem.Requests) != 1 || !strings.HasPrefix(mem.Requests[0], "GET .git/git3/HEAD") {
		t.Fatalf("no-op trace: %#v", mem.Requests)
	}
	if e = os.WriteFile(filepath.Join(src, "hello"), []byte("two\n"), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, src, "commit", "-qam", "two")
	two := run(t, src, "rev-parse", "HEAD")
	if _, e = writer.Push(ctx, nil, []PushCommand{{Dst: "refs/heads/main", NewOID: &two}}, PushOptions{Atomic: true}); e != nil {
		t.Fatal(e)
	}
	s, e = reader.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = reader.Fetch(ctx, advertisement(s), []string{two}, FetchOptions{}); e != nil {
		t.Fatal(e)
	}
	if !reader.Git.HasObject(ctx, two) {
		t.Fatal("incremental object not installed")
	}
	budgetState, e := writer.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = writer.Maintenance(ctx, MaintenanceOptions{Fanout: 4, MaxBytes: int64(budgetState.Transactions[0].ObjectData.Object.Size)}); e != nil {
		t.Fatal(e)
	}
	mid, e := writer.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if mid.Head.Packset.Generation != 1 || mid.Head.LogicalGeneration != 2 {
		t.Fatalf("budgeted maintenance selected generation %d", mid.Head.Packset.Generation)
	}
	if _, e = writer.Maintenance(ctx, MaintenanceOptions{Fanout: 4}); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(src, "hello"), []byte("three\n"), 0600); e != nil {
		t.Fatal(e)
	}
	run(t, src, "commit", "-qam", "three")
	three := run(t, src, "rev-parse", "HEAD")
	if _, e = writer.Push(ctx, nil, []PushCommand{{Dst: "refs/heads/main", NewOID: &three}}, PushOptions{Atomic: true}); e != nil {
		t.Fatal(e)
	}
	if _, e = writer.Maintenance(ctx, MaintenanceOptions{Fanout: 2}); e != nil {
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
	coldProgress := &recordingProgress{}
	if _, e = coldReader.Fetch(ctx, advertisement(s), []string{three}, FetchOptions{Progress: coldProgress}); e != nil {
		t.Fatalf("fetch did not recover an interrupted pack pair install: %v", e)
	}
	if !coldProgress.completed("Receiving S3 pack data") {
		t.Fatal("cold fetch did not report S3 pack download progress")
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
	if _, e = writer.GCExecute(ctx, time.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
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
	if _, err := seed.Push(ctx, nil, []PushCommand{{Dst: "refs/heads/main", NewOID: &one}}, PushOptions{Atomic: true}); err != nil {
		t.Fatal(err)
	}

	cached := &Repository{Store: mem, Git: gitx.Git{Dir: dir}, Version: "test", CacheID: "tampered-remote"}
	state, err := cached.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cached.Fetch(ctx, advertisement(state), []string{one}, FetchOptions{}); err != nil {
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
	if _, err = writer.Push(ctx, advertisement(advertised), []PushCommand{{Dst: "refs/heads/main", NewOID: &two}}, PushOptions{Atomic: true}); err != nil {
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
	results, err := repo.Push(ctx, nil, []PushCommand{
		{Dst: "refs/heads/main", NewOID: &one},
		{Dst: "refs/heads/.invalid", NewOID: &one},
	}, PushOptions{Atomic: true})
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

func TestGCDryRunRejectsPacksetPointerMismatch(t *testing.T) {
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
	oid := run(t, dir, "rev-parse", "HEAD")

	mem := store.NewMemory()
	repo := &Repository{Store: mem, Git: gitx.Git{Dir: dir}, Version: "test"}
	if _, err := repo.Push(ctx, nil, []PushCommand{{Dst: "refs/heads/main", NewOID: &oid}}, PushOptions{Atomic: true}); err != nil {
		t.Fatal(err)
	}
	state, err := repo.ReadUnconditional(ctx)
	if err != nil {
		t.Fatal(err)
	}
	object, err := mem.Get(ctx, state.Head.Packset.Object.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	var packset model.Packset
	if err = canonical.UnmarshalForward(object.Body, &packset, model.MaxPackset); err != nil {
		t.Fatal(err)
	}
	tamperedID := "00000000-0000-4000-8000-000000000001"
	if packset.PacksetID == tamperedID {
		tamperedID = "00000000-0000-4000-8000-000000000002"
	}
	packset.PacksetID = tamperedID
	packsetBytes, err := canonical.Marshal(packset)
	if err != nil {
		t.Fatal(err)
	}
	mem.Set(state.Head.Packset.Object.Key, packsetBytes)

	head := state.Head
	head.Packset.Object = shaRef(head.Packset.Object.Key, packsetBytes)
	headBytes, err := canonical.Marshal(head)
	if err != nil {
		t.Fatal(err)
	}
	mem.Set(".git/git3/HEAD", headBytes)

	_, err = repo.GCDryRun(ctx, time.Now().Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "packset pointer mismatch") {
		t.Fatalf("GC error = %v, want packset pointer mismatch", err)
	}
}
