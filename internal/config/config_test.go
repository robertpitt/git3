package config

import (
	"os"
	"os/exec"
	"path/filepath"
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
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile != "production" {
		t.Fatalf("profile = %q", c.Profile)
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
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(filepath.Clean(dir)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	c, err := Load("origin")
	if err != nil {
		t.Fatal(err)
	}
	if c.Profile != "account-role" {
		t.Fatalf("profile = %q", c.Profile)
	}
}
