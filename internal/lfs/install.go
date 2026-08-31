package lfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const transferName = "git3-s3"

// GitRunner runs a Git command and returns its standard output.
type GitRunner func(context.Context, ...string) ([]byte, error)

// Install registers the git3 custom transfer without changing unrelated LFS endpoints.
func Install(ctx context.Context, executable string, run GitRunner) error {
	if run == nil {
		return fmt.Errorf("git runner is required")
	}
	abs, err := filepath.Abs(executable)
	if err != nil {
		return err
	}
	if _, err = run(ctx, "lfs", "version"); err != nil {
		return fmt.Errorf("git LFS is required: %w", err)
	}
	if _, err = run(ctx, "lfs", "install"); err != nil {
		return fmt.Errorf("install Git LFS filters and hooks: %w", err)
	}
	if err = verifyPrePushHook(ctx, run); err != nil {
		return err
	}
	settings := [][2]string{
		{"lfs.customtransfer." + transferName + ".path", abs},
		{"lfs.customtransfer." + transferName + ".args", "lfs-transfer"},
		{"lfs.customtransfer." + transferName + ".concurrent", "true"},
		{"lfs.customtransfer." + transferName + ".direction", "both"},
		{"lfs.https://s3/.standalonetransferagent", transferName},
		{"lfs.https://s3/.locksverify", "false"},
	}
	for _, setting := range settings {
		if _, err = run(ctx, "config", "--global", "--replace-all", setting[0], setting[1]); err != nil {
			return fmt.Errorf("configure %s: %w", setting[0], err)
		}
	}
	out, err := run(ctx, "config", "--global", "--get-urlmatch", "lfs.standalonetransferagent", "https://s3///git3-probe.git/info/lfs")
	if err != nil || strings.TrimSpace(string(out)) != transferName {
		if err == nil {
			err = fmt.Errorf("resolved agent %q", strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("verify Git LFS transfer registration: %w", err)
	}
	return nil
}

func verifyPrePushHook(ctx context.Context, run GitRunner) error {
	out, err := run(ctx, "rev-parse", "--git-path", "hooks/pre-push")
	if err != nil {
		// Installation outside a repository can only configure the global filters.
		return nil
	}
	hook := strings.TrimSpace(string(out))
	if hook == "" {
		return fmt.Errorf("verify Git LFS pre-push hook: Git returned an empty hook path")
	}
	if !filepath.IsAbs(hook) {
		hook, err = filepath.Abs(hook)
		if err != nil {
			return fmt.Errorf("verify Git LFS pre-push hook: %w", err)
		}
	}
	contents, err := os.ReadFile(hook)
	if err != nil {
		return fmt.Errorf("verify Git LFS pre-push hook: %w", err)
	}
	info, err := os.Stat(hook)
	if err != nil {
		return fmt.Errorf("verify Git LFS pre-push hook: %w", err)
	}
	if info.Mode()&0111 == 0 || !strings.Contains(string(contents), "git lfs pre-push") {
		return fmt.Errorf("verify Git LFS pre-push hook: %s is not an executable Git LFS hook", hook)
	}
	return nil
}
