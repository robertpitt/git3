package helper

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/robertpitt/git3/internal/engine"
	"github.com/robertpitt/git3/internal/store"
)

type absentRemote struct{}

func (absentRemote) Advertise(context.Context) (*engine.Advertisement, error) {
	return nil, store.ErrNotFound
}
func (absentRemote) Fetch(context.Context, *engine.Advertisement, []string, engine.FetchOptions) (engine.FetchReport, error) {
	return engine.FetchReport{}, nil
}
func (absentRemote) Push(context.Context, *engine.Advertisement, []engine.PushCommand, engine.PushOptions) ([]engine.PushResult, error) {
	return nil, nil
}

type progressRemote struct{}

func (progressRemote) Advertise(context.Context) (*engine.Advertisement, error) {
	return &engine.Advertisement{ObjectFormat: "sha1", Refs: map[string]string{"refs/heads/main": strings.Repeat("1", 40)}}, nil
}

func (progressRemote) Fetch(_ context.Context, _ *engine.Advertisement, _ []string, options engine.FetchOptions) (engine.FetchReport, error) {
	if options.Progress != nil {
		_, _ = options.Progress.Write([]byte("Receiving objects: 100% (1/1), done.\n"))
		options.Progress.Update(engine.ProgressEvent{Phase: "Receiving S3 pack data", Current: 1024, Total: 1024, Unit: engine.ProgressUnitBytes, Done: true})
	}
	return engine.FetchReport{}, nil
}

func (progressRemote) Push(_ context.Context, _ *engine.Advertisement, commands []engine.PushCommand, options engine.PushOptions) ([]engine.PushResult, error) {
	if options.Progress != nil {
		_, _ = options.Progress.Write([]byte("Enumerating objects: 1, done.\n"))
		options.Progress.Update(engine.ProgressEvent{Phase: "Publishing refs", Done: true})
	}
	results := make([]engine.PushResult, len(commands))
	for i, command := range commands {
		results[i] = engine.PushResult{Dst: command.Dst, OK: true}
	}
	return results, nil
}

func TestCapabilitiesOptionsAndAbsentList(t *testing.T) {
	in := strings.NewReader("capabilities\noption atomic true\noption dry-run true\noption depth 1\nlist\n")
	var out, errout bytes.Buffer
	h := &Helper{Repo: absentRemote{}, In: in, Out: &out, Err: &errout}
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
	if !h.dryRun {
		t.Fatal("dry-run was not applied")
	}
}

func TestProgressUsesStderrAndRespectsProgressOption(t *testing.T) {
	oid := strings.Repeat("1", 40)
	input := "option progress true\nlist\nfetch " + oid + " refs/heads/main\n\npush refs/heads/main:refs/heads/main\n\n"
	var out, errout bytes.Buffer
	h := &Helper{Repo: progressRemote{}, In: strings.NewReader(input), Out: &out, Err: &errout}
	if err := h.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Resolving S3 repository state: done.", "Receiving objects:", "Receiving S3 pack data:", "Enumerating objects:", "Publishing refs: done."} {
		if !strings.Contains(errout.String(), text) {
			t.Fatalf("progress output missing %q: %q", text, errout.String())
		}
		if strings.Contains(out.String(), text) {
			t.Fatalf("progress leaked to protocol stdout: %q", out.String())
		}
	}

	var quietOut, quietErr bytes.Buffer
	quiet := &Helper{Repo: progressRemote{}, In: strings.NewReader("option progress false\nlist\nfetch " + oid + " refs/heads/main\n\n"), Out: &quietOut, Err: &quietErr}
	if err := quiet.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if quietErr.Len() != 0 {
		t.Fatalf("quiet operation emitted progress: %q", quietErr.String())
	}
}
