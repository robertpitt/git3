package helper

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/robertpitt/git3/internal/engine"
	"github.com/robertpitt/git3/internal/store"
)

func TestCapabilitiesOptionsAndAbsentList(t *testing.T) {
	in := strings.NewReader("capabilities\noption atomic true\noption dry-run true\noption depth 1\nlist\n")
	var out, errout bytes.Buffer
	repo := &engine.Repository{Store: store.NewMemory()}
	h := &Helper{Repo: repo, In: in, Out: &out, Err: &errout}
	if e := h.Run(context.Background()); e != nil {
		t.Fatal(e)
	}
	want := "fetch\npush\noption\ncheck-connectivity\nobject-format\n\nok\nok\nerror shallow and partial operation is unsupported\n\n"
	if out.String() != want {
		t.Fatalf("got:\n%q\nwant:\n%q", out.String(), want)
	}
	if errout.Len() != 0 {
		t.Fatalf("protocol diagnostics leaked: %s", errout.String())
	}
	if !repo.DryRun {
		t.Fatal("dry-run was not applied")
	}
}
