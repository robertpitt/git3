package main

import (
	"testing"
	"time"
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
