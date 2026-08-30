package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"1": 1, "5KiB": 5 << 10, "2MiB": 2 << 20, "3GiB": 3 << 30, "1TiB": 1 << 40} {
		got, e := ParseBytes(in)
		if e != nil || got != want {
			t.Fatalf("%s: %d %v", in, got, e)
		}
	}
	for _, x := range []string{"", "-1", "1KB", "1.5MiB"} {
		if _, e := ParseBytes(x); e == nil {
			t.Errorf("accepted %q", x)
		}
	}
}
func TestDefaultsValidate(t *testing.T) {
	if e := Defaults().Validate(); e != nil {
		t.Fatal(e)
	}
	c := Defaults()
	c.PartSize = 5 << 20
	if e := c.Validate(); e == nil {
		t.Fatal("accepted part size unable to address 1 TiB")
	}
	c = Defaults()
	c.Endpoint = "http://example.com"
	if e := c.Validate(); e == nil {
		t.Fatal("accepted insecure endpoint")
	}
}

func TestProfileFromAWSProfile(t *testing.T) {
	t.Setenv("AWS_PROFILE", "production")
	t.Setenv("GIT3_UPLOAD_CONCURRENCY", "5")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile != "production" {
		t.Fatalf("profile = %q", c.Profile)
	}
	if c.UploadConcurrency != 5 {
		t.Fatalf("upload concurrency = %d", c.UploadConcurrency)
	}
}

func TestNamedRemoteProfile(t *testing.T) {
	t.Setenv("AWS_PROFILE", "temporary-value-for-cleanup")
	if err := os.Unsetenv("AWS_PROFILE"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "config", "remote.origin.git3Profile", "account-role").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "config", "git3.profile", "default-role").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(filepath.Clean(dir)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	trace := filepath.Join(bin, "calls")
	script := "#!/bin/sh\nprintf x >> \"$GIT3_TEST_TRACE\"\nexec \"$GIT3_TEST_GIT\" \"$@\"\n"
	if err = os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT3_TEST_TRACE", trace)
	t.Setenv("GIT3_TEST_GIT", realGit)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c, err := Load("origin")
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile != "account-role" {
		t.Fatalf("profile = %q", c.Profile)
	}
	t.Setenv("AWS_PROFILE", "environment-role")
	c, err = Load("origin")
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile != "environment-role" {
		t.Fatalf("environment profile = %q", c.Profile)
	}
	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "x"); got != 2 {
		t.Fatalf("git config subprocesses = %d, want 2", got)
	}
}
