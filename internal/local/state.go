package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/robertpitt/git3/internal/canonical"
	gitx "github.com/robertpitt/git3/internal/git"
	"github.com/robertpitt/git3/internal/model"
)

// State identifies repository-local cache and pack directories.
type State struct{ Root, PackDir string }

// Resolve locates local state for repoID in the current Git repository.
func Resolve(ctx context.Context, g gitx.Git, repoID string) (State, error) {
	root, e := g.GitPath(ctx, "git3/"+repoID)
	if e != nil {
		return State{}, e
	}
	packs, e := g.PackDir(ctx)
	if e != nil {
		return State{}, e
	}
	return State{Root: root, PackDir: packs}, nil
}

// Lock serializes updates to repository-local state.
type Lock struct {
	path string
	f    *os.File
}

// Lock acquires the repository-local state lock.
func (s State) Lock(ctx context.Context) (*Lock, error) {
	if e := os.MkdirAll(s.Root, 0700); e != nil {
		return nil, e
	}
	p := filepath.Join(s.Root, "state.lock")
	deadline := time.Now().Add(30 * time.Second)
	for {
		f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			return &Lock{p, f}, nil
		}
		if !errors.Is(e, os.ErrExist) {
			return nil, e
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("local state lock timeout")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Close releases the repository-local state lock.
func (l *Lock) Close() error {
	_ = l.f.Close()
	return os.Remove(l.path)
}
func atomicWrite(path string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".git3-tmp-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	if d, e := os.Open(filepath.Dir(path)); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ReadCursor reads and validates the cached remote cursor.
func (s State) ReadCursor() (model.Cursor, error) {
	b, e := os.ReadFile(filepath.Join(s.Root, "cursor.json"))
	if e != nil {
		return model.Cursor{}, e
	}
	var c model.Cursor
	e = canonical.UnmarshalStrict(b, &c, 1<<20)
	return c, e
}
func (s State) Write(c model.Cursor, refs []byte, etag string) error {
	sum := sha256.Sum256(refs)
	c.CachedRefsSHA256 = hex.EncodeToString(sum[:])
	cb, e := canonical.Marshal(c)
	if e != nil {
		return e
	}
	if e = atomicWrite(filepath.Join(s.Root, "refs.snapshot"), refs); e != nil {
		return e
	}
	if e = atomicWrite(filepath.Join(s.Root, "head.etag"), []byte(etag)); e != nil {
		return e
	}
	return atomicWrite(filepath.Join(s.Root, "cursor.json"), cb)
}

// WriteHead atomically caches a serialized remote HEAD document.
func (s State) WriteHead(b []byte) error {
	return atomicWrite(filepath.Join(s.Root, "remote-head.json"), b)
}

// ReadHead reads the cached remote HEAD document.
func (s State) ReadHead() ([]byte, error) {
	return os.ReadFile(filepath.Join(s.Root, "remote-head.json"))
}

// WriteRemoteMapping records the repository ID associated with a remote locator.
func WriteRemoteMapping(ctx context.Context, g gitx.Git, cacheID, repoID string) error {
	p, e := g.GitPath(ctx, "git3/remotes/"+cacheID)
	if e != nil {
		return e
	}
	return atomicWrite(p, []byte(repoID+"\n"))
}

// ReadRemoteMapping returns the repository ID associated with a remote locator.
func ReadRemoteMapping(ctx context.Context, g gitx.Git, cacheID string) (string, error) {
	p, e := g.GitPath(ctx, "git3/remotes/"+cacheID)
	if e != nil {
		return "", e
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return "", e
	}
	return string(bytes.TrimSpace(b)), nil
}

// CachedRefs reads the cached ref snapshot after verifying its recorded digest.
func (s State) CachedRefs() ([]byte, error) {
	b, e := os.ReadFile(filepath.Join(s.Root, "refs.snapshot"))
	if e != nil {
		return nil, e
	}
	c, e := s.ReadCursor()
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != c.CachedRefsSHA256 {
		return nil, fmt.Errorf("cached refs checksum mismatch")
	}
	return b, nil
}
