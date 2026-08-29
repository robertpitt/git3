package model

import "testing"

func TestSnapshotRoundTrip(t *testing.T) {
	id := "73bcb050-8b53-4e47-a5a4-e661bd5c8faf"
	tx := "2bc4993f-404d-419d-8c28-b6c7427fce37"
	s := Snapshot{RepositoryID: id, ObjectFormat: "sha1", Generation: 1, TransactionID: &tx, Refs: map[string]string{"refs/heads/main": "1111111111111111111111111111111111111111"}}
	b, e := s.MarshalText()
	if e != nil {
		t.Fatal(e)
	}
	got, e := ParseSnapshot(b)
	if e != nil {
		t.Fatal(e)
	}
	if got.Refs["refs/heads/main"] != s.Refs["refs/heads/main"] {
		t.Fatal("round trip mismatch")
	}
}
func TestSnapshotRejectsUnsorted(t *testing.T) {
	b := []byte("git3-ref-snapshot 1\nrepository 73bcb050-8b53-4e47-a5a4-e661bd5c8faf\nobject-format sha1\ngeneration 0\ntransaction -\n\n1111111111111111111111111111111111111111 refs/heads/z\n1111111111111111111111111111111111111111 refs/heads/a\n")
	if _, e := ParseSnapshot(b); e == nil {
		t.Fatal("accepted unsorted refs")
	}
}
