package errs

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCodeOfUsesTypedCauseBeforeMessageHeuristics(t *testing.T) {
	err := E(LocalGitFailed, "git", errors.New("authorization checksum invalid"))
	if got := CodeOf(fmt.Errorf("operation failed: %w", err)); got != LocalGitFailed {
		t.Fatalf("CodeOf = %s, want %s", got, LocalGitFailed)
	}
}

func TestCodeOfRecognizesWrappedCancellation(t *testing.T) {
	if got := CodeOf(fmt.Errorf("stopped: %w", context.Canceled)); got != Cancelled {
		t.Fatalf("CodeOf = %s, want %s", got, Cancelled)
	}
}

func TestWithDefaultPreservesExistingClassification(t *testing.T) {
	err := E(AuthFailed, "s3", errors.New("denied"))
	if got := CodeOf(WithDefault(RepositoryCorrupt, "read", err)); got != AuthFailed {
		t.Fatalf("CodeOf = %s, want %s", got, AuthFailed)
	}
}
