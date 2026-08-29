package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitx "github.com/robertpitt/git3/internal/git"
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
	if e = coldReader.Fetch(ctx, s, []string{three}, true); e != nil {
		t.Fatal(e)
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
