package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestMemoryStoreContract(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	metadata, err := store.Put(ctx, "objects/a", strings.NewReader("abcdef"), 6, PutOptions{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Put(ctx, "objects/a", strings.NewReader("other"), 5, PutOptions{IfNoneMatch: true}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("duplicate put = %v", err)
	}
	object, err := store.Get(ctx, "objects/a", "")
	if err != nil || !bytes.Equal(object.Body, []byte("abcdef")) || object.ETag != metadata.ETag {
		t.Fatalf("get = %#v, %v", object, err)
	}
	if _, err = store.Get(ctx, "objects/a", metadata.ETag); !errors.Is(err, ErrNotModified) {
		t.Fatalf("conditional get = %v", err)
	}
	part, err := store.OpenRange(ctx, "objects/a", 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	partBody, err := io.ReadAll(part.Body)
	_ = part.Body.Close()
	if err != nil || string(partBody) != "bcd" || part.Size != 3 || part.TotalSize != 6 {
		t.Fatalf("range = %q size=%d total=%d, %v", partBody, part.Size, part.TotalSize, err)
	}
	if _, err = store.OpenRange(ctx, "objects/a", 3, 9); err == nil {
		t.Fatal("accepted invalid range")
	}
	head, err := store.Head(ctx, "objects/a")
	if err != nil || head.Size != 6 || head.ETag != metadata.ETag {
		t.Fatalf("head = %#v, %v", head, err)
	}
	store.Set("objects/b", []byte("b"))
	var listed []Metadata
	err = store.Walk(ctx, "objects/", func(metadata Metadata) error {
		listed = append(listed, metadata)
		return nil
	})
	if err != nil || len(listed) != 2 || listed[0].Key != "objects/a" || listed[1].Key != "objects/b" {
		t.Fatalf("list = %#v, %v", listed, err)
	}
	if err = store.Delete(ctx, "objects/a", DeleteOptions{IfMatch: "wrong"}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("conditional delete = %v", err)
	}
	if err = store.Delete(ctx, "objects/a", DeleteOptions{IfMatch: metadata.ETag}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get(ctx, "objects/a", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted get = %v", err)
	}
	if _, err = store.Put(ctx, "objects/c", strings.NewReader("bad-size"), 1, PutOptions{}); err == nil {
		t.Fatal("accepted size mismatch")
	}
}
