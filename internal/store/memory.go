package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

type memoryEntry struct {
	body     []byte
	etag     string
	modified time.Time
}

// Memory is a concurrency-safe in-memory Store used by tests.
type Memory struct {
	mu       sync.Mutex
	objects  map[string]memoryEntry
	Requests []string
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{objects: map[string]memoryEntry{}} }

// Get returns the current object or validates a conditional ETag.
func (m *Memory) Get(_ context.Context, key, ifNone string) (Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requests = append(m.Requests, "GET "+key)
	e, ok := m.objects[key]
	if !ok {
		return Object{}, ErrNotFound
	}
	if ifNone != "" && ifNone == e.etag {
		return Object{}, ErrNotModified
	}
	return Object{Body: append([]byte(nil), e.body...), ETag: e.etag, Size: int64(len(e.body)), LastModified: e.modified}, nil
}

// OpenRange opens the inclusive byte range [start, end].
func (m *Memory) OpenRange(_ context.Context, key string, start, end int64) (Range, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requests = append(m.Requests, "RANGE "+key)
	e, ok := m.objects[key]
	if !ok {
		return Range{}, ErrNotFound
	}
	if start < 0 || end < start || end >= int64(len(e.body)) {
		return Range{}, fmt.Errorf("invalid range")
	}
	b := append([]byte(nil), e.body[start:end+1]...)
	return Range{Body: io.NopCloser(bytes.NewReader(b)), Size: int64(len(b)), TotalSize: int64(len(e.body))}, nil
}

// Head returns metadata for the current object.
func (m *Memory) Head(_ context.Context, key string) (Metadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requests = append(m.Requests, "HEAD "+key)
	e, ok := m.objects[key]
	if !ok {
		return Metadata{}, ErrNotFound
	}
	return Metadata{Key: key, ETag: e.etag, Size: int64(len(e.body)), LastModified: e.modified}, nil
}

// Put stores an object when its supplied preconditions hold.
func (m *Memory) Put(_ context.Context, key string, r io.Reader, size int64, o PutOptions) (Metadata, error) {
	b, e := io.ReadAll(r)
	if e != nil {
		return Metadata{}, e
	}
	if size >= 0 && int64(len(b)) != size {
		return Metadata{}, fmt.Errorf("size mismatch")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requests = append(m.Requests, "PUT "+key)
	old, exists := m.objects[key]
	if o.IfNoneMatch && exists {
		return Metadata{}, ErrPrecondition
	}
	if o.IfMatch != "" && (!exists || old.etag != o.IfMatch) {
		return Metadata{}, ErrPrecondition
	}
	sum := sha256.Sum256(b)
	etag := fmt.Sprintf("\"%x\"", sum[:16])
	now := time.Now().UTC()
	m.objects[key] = memoryEntry{append([]byte(nil), b...), etag, now}
	return Metadata{Key: key, ETag: etag, Size: int64(len(b)), LastModified: now}, nil
}

// Delete removes an object when its supplied precondition holds.
func (m *Memory) Delete(_ context.Context, key string, o DeleteOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requests = append(m.Requests, "DELETE "+key)
	old, ok := m.objects[key]
	if !ok {
		return ErrNotFound
	}
	if o.IfMatch != "" && o.IfMatch != old.etag {
		return ErrPrecondition
	}
	delete(m.objects, key)
	return nil
}

// Walk visits object metadata beneath prefix in key order.
func (m *Memory) Walk(ctx context.Context, prefix string, visit func(Metadata) error) error {
	m.mu.Lock()
	m.Requests = append(m.Requests, "LIST "+prefix)
	var out []Metadata
	for k, e := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Metadata{Key: k, ETag: e.etag, Size: int64(len(e.body)), LastModified: e.modified})
		}
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	for _, metadata := range out {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := visit(metadata); e != nil {
			return e
		}
	}
	return nil
}

// Set unconditionally stores an object for test setup.
func (m *Memory) Set(key string, b []byte) {
	_, _ = m.Put(context.Background(), key, bytes.NewReader(b), int64(len(b)), PutOptions{})
}
