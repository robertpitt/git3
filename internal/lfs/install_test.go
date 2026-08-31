package lfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInstallRegistersURLScopedAgent(t *testing.T) {
	var calls [][]string
	hook := filepath.Join(t.TempDir(), "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ngit lfs pre-push \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if reflect.DeepEqual(args, []string{"rev-parse", "--git-path", "hooks/pre-push"}) {
			return []byte(hook + "\n"), nil
		}
		if len(args) > 2 && args[0] == "config" && args[2] == "--get-urlmatch" {
			return []byte(transferName + "\n"), nil
		}
		return nil, nil
	}
	if err := Install(context.Background(), "./git3", run); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 10 {
		t.Fatalf("calls = %#v", calls)
	}
	if !reflect.DeepEqual(calls[0], []string{"lfs", "version"}) || !reflect.DeepEqual(calls[1], []string{"lfs", "install"}) {
		t.Fatalf("Git LFS setup calls = %#v", calls[:2])
	}
	joined := make([]string, len(calls))
	for i := range calls {
		joined[i] = strings.Join(calls[i], " ")
	}
	all := strings.Join(joined, "\n")
	for _, required := range []string{
		"lfs.customtransfer.git3-s3.path",
		"lfs.customtransfer.git3-s3.args lfs-transfer",
		"lfs.customtransfer.git3-s3.direction both",
		"lfs.https://s3/.standalonetransferagent git3-s3",
		"lfs.https://s3/.locksverify false",
	} {
		if !strings.Contains(all, required) {
			t.Fatalf("missing %q in calls:\n%s", required, all)
		}
	}
	if strings.Contains(all, "lfs.standalonetransferagent git3-s3") {
		t.Fatalf("agent registration was not URL-scoped:\n%s", all)
	}
}

func TestInstallRejectsMissingPrePushHook(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"rev-parse", "--git-path", "hooks/pre-push"}) {
			return []byte(filepath.Join(t.TempDir(), "missing") + "\n"), nil
		}
		return nil, nil
	}
	err := Install(context.Background(), "./git3", run)
	if err == nil || !strings.Contains(err.Error(), "pre-push hook") {
		t.Fatalf("Install error = %v", err)
	}
}

func TestURLScopedAgentMatchesOnlyGitLFSDerivedS3Endpoint(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config")
	set := exec.Command("git", "config", "--file", config, "lfs.https://s3/.standalonetransferagent", transferName)
	if output, err := set.CombinedOutput(); err != nil {
		t.Fatalf("set config: %v: %s", err, output)
	}
	match := func(url string) string {
		t.Helper()
		command := exec.Command("git", "config", "--file", config, "--get-urlmatch", "lfs.standalonetransferagent", url)
		output, _ := command.Output()
		return strings.TrimSpace(string(output))
	}
	if got := match("https://s3///bucket/repository.git/info/lfs"); got != transferName {
		t.Fatalf("S3 match = %q", got)
	}
	if got := match("https://github.com/owner/repository.git/info/lfs"); got != "" {
		t.Fatalf("unrelated HTTPS match = %q", got)
	}
}
