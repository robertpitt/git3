package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gitx "github.com/robertpitt/git3/internal/git"
	"github.com/robertpitt/git3/internal/store"
)

func TestSHA256RepositoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	run(t, src, "init", "-q", "--object-format=sha256", "-b", "main")
	run(t, src, "config", "user.email", "test@example.com")
	run(t, src, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(src, "f"), []byte("sha256\n"), 0600)
	run(t, src, "add", "f")
	run(t, src, "commit", "-qm", "sha256")
	oid := run(t, src, "rev-parse", "HEAD")
	mem := store.NewMemory()
	w := &Repository{Store: mem, Git: gitx.Git{Dir: src}, Version: "test"}
	if _, e := w.Push(ctx, []PushCommand{{Dst: "refs/heads/main", NewOID: &oid}}, true); e != nil {
		t.Fatal(e)
	}
	dst := t.TempDir()
	run(t, dst, "init", "--bare", "-q", "--object-format=sha256")
	r := &Repository{Store: mem, Git: gitx.Git{Dir: dst}, Version: "test"}
	s, e := r.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if s.Head.ObjectFormat != "sha256" {
		t.Fatal("remote format mismatch")
	}
	if e = r.Fetch(ctx, s, []string{oid}, true); e != nil {
		t.Fatal(e)
	}
	if !r.Git.HasObject(ctx, oid) {
		t.Fatal("SHA-256 object not installed")
	}
}
