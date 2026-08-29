package store

import (
	"context"
	"errors"
	"io"
	"time"
)

// Sentinel errors returned by Store implementations.
var (
	ErrNotFound     = errors.New("object not found")
	ErrNotModified  = errors.New("object not modified")
	ErrPrecondition = errors.New("precondition failed")
)

// Object contains an object's bytes and validated metadata.
type Object struct {
	Body         []byte
	ETag         string
	Size         int64
	LastModified time.Time
}

// Metadata describes an object without its body.
type Metadata struct {
	Key, ETag    string
	Size         int64
	LastModified time.Time
}

// PutOptions supplies conditional-write and integrity requirements.
type PutOptions struct {
	IfMatch       string
	IfNoneMatch   bool
	ContentSHA256 string
}

// DeleteOptions supplies a conditional-delete precondition.
type DeleteOptions struct{ IfMatch string }

// Store is the object-storage contract required by the repository engine.
type Store interface {
	Get(context.Context, string, string) (Object, error)
	GetRange(context.Context, string, int64, int64) ([]byte, error)
	Head(context.Context, string) (Metadata, error)
	Put(context.Context, string, io.Reader, int64, PutOptions) (Metadata, error)
	Delete(context.Context, string, DeleteOptions) error
	List(context.Context, string) ([]Metadata, error)
}
