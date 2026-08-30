package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	gitx "github.com/robertpitt/git3/internal/git"
	"github.com/robertpitt/git3/internal/store"
)

type lostResponseStore struct {
	*store.Memory
	lose bool
}

func (s *lostResponseStore) Put(ctx context.Context, key string, r io.Reader, size int64, o store.PutOptions) (store.Metadata, error) {
	m, e := s.Memory.Put(ctx, key, r, size, o)
	if e == nil && s.lose && key == ".git/git3/HEAD" && o.IfMatch != "" {
		s.lose = false
		return store.Metadata{}, fmt.Errorf("simulated lost response")
	}
	return m, e
}

func TestEightPinnedWritersOneCASWinner(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(dir, "f"), []byte("a"), 0600)
	run(t, dir, "add", "f")
	run(t, dir, "commit", "-qm", "a")
	a := run(t, dir, "rev-parse", "HEAD")
	mem := store.NewMemory()
	first := &Repository{Store: mem, Git: gitx.Git{Dir: dir}, Version: "test"}
	if _, e := first.Push(ctx, nil, []PushCommand{{Dst: "refs/heads/main", NewOID: &a}}, PushOptions{Atomic: true}); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(dir, "f"), []byte("b"), 0600)
	run(t, dir, "commit", "-qam", "b")
	b := run(t, dir, "rev-parse", "HEAD")
	writers := make([]*Repository, 8)
	advertisements := make([]*Advertisement, len(writers))
	for i := range writers {
		writers[i] = &Repository{Store: mem, Git: gitx.Git{Dir: dir}, Version: "test"}
		var e error
		advertisements[i], e = writers[i].Advertise(ctx)
		if e != nil {
			t.Fatal(e)
		}
	}
	var wg sync.WaitGroup
	wins := 0
	var mu sync.Mutex
	for i, w := range writers {
		wg.Add(1)
		go func(w *Repository, advertised *Advertisement) {
			defer wg.Done()
			_, e := w.Push(ctx, advertised, []PushCommand{{Dst: "refs/heads/main", NewOID: &b}}, PushOptions{Atomic: true})
			if e == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(w, advertisements[i])
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("got %d CAS winners", wins)
	}
	s, e := first.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if s.Head.LogicalGeneration != 2 || s.Refs["refs/heads/main"] != b {
		t.Fatalf("lost update: %#v", s.Head)
	}
}

func TestLostPublishResponseIsConfirmedFromChain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(dir, "f"), []byte("a"), 0600)
	run(t, dir, "add", "f")
	run(t, dir, "commit", "-qm", "a")
	a := run(t, dir, "rev-parse", "HEAD")
	st := &lostResponseStore{Memory: store.NewMemory()}
	repo := &Repository{Store: st, Git: gitx.Git{Dir: dir}, Version: "test"}
	if _, e := repo.Push(ctx, nil, []PushCommand{{Dst: "refs/heads/main", NewOID: &a}}, PushOptions{Atomic: true}); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(dir, "f"), []byte("b"), 0600)
	run(t, dir, "commit", "-qam", "b")
	b := run(t, dir, "rev-parse", "HEAD")
	advertised, e := repo.Advertise(ctx)
	if e != nil {
		t.Fatal(e)
	}
	st.lose = true
	if res, e := repo.Push(ctx, advertised, []PushCommand{{Dst: "refs/heads/main", NewOID: &b}}, PushOptions{Atomic: true}); e != nil || !res[0].OK {
		t.Fatalf("publication was not confirmed: %#v %v", res, e)
	}
	s, e := repo.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if s.Refs["refs/heads/main"] != b || s.Head.LogicalGeneration != 2 {
		t.Fatal("published state missing")
	}
	st.lose = true
	publicationID, e := repo.SetHead(ctx, "refs/heads/main")
	if e != nil {
		t.Fatalf("set-head publication was not confirmed: %v", e)
	}
	s, e = repo.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if s.Head.PublicationID != publicationID {
		t.Fatal("confirmed set-head publication is missing")
	}
}
