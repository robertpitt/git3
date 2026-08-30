package local

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gitx "github.com/robertpitt/git3/internal/git"
	"github.com/robertpitt/git3/internal/model"
)

func TestLockRecoversStaleLockFile(t *testing.T) {
	state := State{Root: t.TempDir()}
	lockPath := filepath.Join(state.Root, "state.lock")
	if err := os.WriteFile(lockPath, []byte("999999999\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	lock, err := state.Lock(ctx)
	if err != nil {
		t.Fatalf("stale lock was not recovered: %v", err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLockSerializesCallers(t *testing.T) {
	state := State{Root: t.TempDir()}
	first, err := state.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err = state.Lock(ctx); err == nil {
		t.Fatal("second caller acquired held lock")
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := state.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStateCacheRoundTripAndCorruption(t *testing.T) {
	state := State{Root: t.TempDir()}
	refs := []byte("cached refs\n")
	cursor := model.Cursor{FormatVersion: 1, RepositoryID: "repo", LastHeadETag: "etag"}
	if err := state.Write(cursor, refs, "etag"); err != nil {
		t.Fatal(err)
	}
	readCursor, err := state.ReadCursor()
	if err != nil {
		t.Fatal(err)
	}
	if readCursor.CachedRefsSHA256 == "" || readCursor.LastHeadETag != "etag" {
		t.Fatalf("cursor = %#v", readCursor)
	}
	readRefs, err := state.CachedRefs()
	if err != nil || !bytes.Equal(readRefs, refs) {
		t.Fatalf("refs = %q, %v", readRefs, err)
	}
	head := []byte(`{"formatVersion":1}`)
	if err = state.WriteHead(head); err != nil {
		t.Fatal(err)
	}
	readHead, err := state.ReadHead()
	if err != nil || !bytes.Equal(readHead, head) {
		t.Fatalf("head = %q, %v", readHead, err)
	}
	if err = os.WriteFile(filepath.Join(state.Root, "refs.snapshot"), []byte("tampered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = state.CachedRefs(); err == nil {
		t.Fatal("accepted tampered cached refs")
	}
}

func TestRemoteMappingRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	git := gitx.Git{}
	if _, err := git.Run(ctx, "init", "--bare", dir); err != nil {
		t.Fatal(err)
	}
	git.Dir = dir
	if err := WriteRemoteMapping(ctx, git, "cache", "repository-id"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRemoteMapping(ctx, git, "cache")
	if err != nil || got != "repository-id" {
		t.Fatalf("mapping = %q, %v", got, err)
	}
}
