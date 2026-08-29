package model

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/robertpitt/git3/internal/canonical"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", name))
	if e != nil {
		t.Fatal(e)
	}
	return bytes.TrimSuffix(b, []byte("\n"))
}
func TestGoldenJSONFixtures(t *testing.T) {
	var tx Transaction
	b := fixture(t, "transaction-v1.json")
	if e := canonical.UnmarshalForward(b, &tx, MaxTransaction); e != nil {
		t.Fatal(e)
	}
	if e := tx.Validate(); e != nil {
		t.Fatal(e)
	}
	out, e := canonical.Marshal(tx)
	if e != nil || !bytes.Equal(out, b) {
		t.Fatalf("transaction fixture is not canonical: %s %v", out, e)
	}
	var ps Packset
	b = fixture(t, "packset-v1.json")
	if e = canonical.UnmarshalForward(b, &ps, MaxPackset); e != nil {
		t.Fatal(e)
	}
	if e = ps.Validate(); e != nil {
		t.Fatal(e)
	}
	out, e = canonical.Marshal(ps)
	if e != nil || !bytes.Equal(out, b) {
		t.Fatalf("packset fixture is not canonical: %s %v", out, e)
	}
}
func TestCorruptFixtureRejected(t *testing.T) {
	var v map[string]any
	if e := canonical.UnmarshalForward(fixture(t, "corrupt-duplicate-key.json"), &v, 1024); e == nil {
		t.Fatal("duplicate key fixture accepted")
	}
}
