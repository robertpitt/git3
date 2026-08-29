package git

import (
	"context"
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
