package model

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestHeadPreservesUnknownOptionalFields(t *testing.T) {
	var h Head
	if e := json.Unmarshal([]byte(`{"formatVersion":1,"future":{"enabled":true}}`), &h); e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(h)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(b, []byte(`"future":{"enabled":true}`)) {
		t.Fatalf("unknown field lost: %s", b)
	}
}
