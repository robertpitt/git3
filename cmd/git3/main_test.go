package main

import (
	"context"
	"os/exec"
	"testing"
	"time"

	gitx "github.com/robertpitt/git3/internal/git"
)

func TestParseCutoff(t *testing.T) {
	before := time.Now().UTC().Add(-30*24*time.Hour - time.Second)
	after := time.Now().UTC().Add(-30*24*time.Hour + time.Second)
	got, e := parseCutoff("30d")
	if e != nil || got.Before(before) || got.After(after) {
		t.Fatalf("unexpected cutoff %v %v", got, e)
	}
	if _, e = parseCutoff("never"); e == nil {
		t.Fatal("accepted invalid cutoff")
	}
}

func TestResolveTargetUsesPushURLOnlyForLFSUploads(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	for _, setting := range [][2]string{
		{"remote.origin.url", "s3://read-bucket/repository"},
		{"remote.origin.pushurl", "s3://write-bucket/repository"},
	} {
		if out, err := exec.Command("git", "-C", dir, "config", setting[0], setting[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config: %v: %s", err, out)
		}
	}
	t.Chdir(dir)
	name, raw, err := resolveTarget(context.Background(), gitx.Git{}, "origin", "download")
	if err != nil || name != "origin" || raw != "s3://read-bucket/repository" {
		t.Fatalf("download target = %q %q, %v", name, raw, err)
	}
	name, raw, err = resolveTarget(context.Background(), gitx.Git{}, "origin", "upload")
	if err != nil || name != "origin" || raw != "s3://write-bucket/repository" {
		t.Fatalf("upload target = %q %q, %v", name, raw, err)
	}
	name, raw, err = resolveTarget(context.Background(), gitx.Git{}, "s3://direct/repository", "upload")
	if err != nil || name != "" || raw != "s3://direct/repository" {
		t.Fatalf("direct target = %q %q, %v", name, raw, err)
	}
}
