package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitDirIgnoresInheritedRepositoryEnvironment(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository.git")
	defaultGit := Git{}
	if _, err := defaultGit.Run(context.Background(), "init", "--bare", repository); err != nil {
		t.Fatalf("initializing bare repository: %v", err)
	}
	t.Setenv("GIT_DIR", ".git")
	t.Setenv("GIT_WORK_TREE", "/not/the/test/repository")

	path, err := (Git{Dir: repository}).GitPath(context.Background(), "git3/test")
	if err != nil {
		t.Fatalf("resolving path in explicit repository: %v", err)
	}
	want := filepath.Join(repository, "git3/test")
	if path != want && !strings.HasSuffix(path, filepath.FromSlash("repository.git/git3/test")) {
		t.Fatalf("GitPath() = %q, want %q", path, want)
	}
}

func TestExistingObjectsBatchesAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "repository.git")
	g := Git{Dir: repository}
	if _, err := (Git{}).Run(ctx, "init", "--bare", repository); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "object")
	if err := os.WriteFile(path, []byte("object\n"), 0600); err != nil {
		t.Fatal(err)
	}
	body, err := g.Run(ctx, "hash-object", "-w", path)
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.TrimSpace(string(body))
	missing := strings.Repeat("0", len(oid))
	existing, err := g.ExistingObjects(ctx, []string{oid, oid, missing, missing})
	if err != nil {
		t.Fatal(err)
	}
	if !existing[oid] || existing[missing] {
		t.Fatalf("existing objects = %#v", existing)
	}
}
