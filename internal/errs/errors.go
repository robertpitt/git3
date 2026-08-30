package errs

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Code identifies a stable class of command failure.
type Code string

// Stable command failure codes.
const (
	ConfigInvalid       Code = "CONFIG_INVALID"
	AuthFailed          Code = "AUTH_FAILED"
	BucketNotFound      Code = "BUCKET_NOT_FOUND"
	RepositoryCorrupt   Code = "REPOSITORY_CORRUPT"
	FormatUnsupported   Code = "FORMAT_UNSUPPORTED"
	CASConflict         Code = "CAS_CONFLICT"
	GCBarrierActive     Code = "GC_BARRIER_ACTIVE"
	IntegrityFailed     Code = "INTEGRITY_FAILED"
	NetworkExhausted    Code = "NETWORK_EXHAUSTED"
	PublishAmbiguous    Code = "PUBLISH_AMBIGUOUS"
	LocalGitFailed      Code = "LOCAL_GIT_FAILED"
	LocalResourceFailed Code = "LOCAL_RESOURCE_FAILED"
	Cancelled           Code = "CANCELLED"
)

// Error associates an underlying error with a stable code and operation.
type Error struct {
	Code Code
	Op   string
	Err  error
}

func (e *Error) Error() string {
	if e.Op == "" {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Op, e.Err)
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// E wraps err with a stable code and operation name.
func E(code Code, op string, err error) error {
	return &Error{Code: code, Op: op, Err: err}
}

// WithDefault classifies err only when a deeper layer has not already done so.
func WithDefault(code Code, op string, err error) error {
	var classified *Error
	if errors.As(err, &classified) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return E(code, op, err)
}

// ExitCode maps a stable failure code to its command exit status.
func ExitCode(code Code) int {
	switch code {
	case ConfigInvalid:
		return 2
	case AuthFailed, BucketNotFound:
		return 3
	case RepositoryCorrupt, FormatUnsupported:
		return 4
	case CASConflict, GCBarrierActive:
		return 5
	case IntegrityFailed:
		return 6
	case LocalGitFailed, LocalResourceFailed:
		return 7
	case PublishAmbiguous:
		return 8
	default:
		return 1
	}
}

// CodeOf classifies err into a stable command failure code.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Cancelled
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "cancel"):
		return Cancelled
	case strings.Contains(s, "cas conflict") || strings.Contains(s, "precondition"):
		return CASConflict
	case strings.Contains(s, "gc barrier"):
		return GCBarrierActive
	case strings.Contains(s, "integrity") || strings.Contains(s, "checksum"):
		return IntegrityFailed
	case strings.Contains(s, "authorization") || strings.Contains(s, "credential") || strings.Contains(s, "access denied"):
		return AuthFailed
	case strings.Contains(s, "bucket not found"):
		return BucketNotFound
	case strings.Contains(s, "object format") || strings.Contains(s, "unsupported format"):
		return FormatUnsupported
	case strings.Contains(s, "git ") || strings.Contains(s, "index-pack") || strings.Contains(s, "pack-objects"):
		return LocalGitFailed
	case strings.Contains(s, "lock") || strings.Contains(s, "disk"):
		return LocalResourceFailed
	case strings.Contains(s, "invalid") || strings.Contains(s, "usage") || strings.Contains(s, "requires"):
		return ConfigInvalid
	default:
		return RepositoryCorrupt
	}
}
