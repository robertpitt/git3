package lfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/robertpitt/git3/internal/store"
)

type seekRequiredStore struct{ *store.Memory }

func (s seekRequiredStore) Put(ctx context.Context, key string, reader io.Reader, size int64, options store.PutOptions) (store.Metadata, error) {
	if _, ok := reader.(io.Seeker); !ok {
		return store.Metadata{}, fmt.Errorf("upload stream is not seekable")
	}
	return s.Memory.Put(ctx, key, reader, size, options)
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func runAgent(t *testing.T, input string, backend Backend) []response {
	t.Helper()
	return runAgentInTemp(t, input, backend, t.TempDir())
}

func runAgentInTemp(t *testing.T, input string, backend Backend, tempDir string) []response {
	t.Helper()
	var output bytes.Buffer
	agent := Agent{
		In:      strings.NewReader(input),
		Out:     &output,
		TempDir: tempDir,
		Open: func(_ context.Context, operation, remote string) (Backend, error) {
			if operation != "upload" && operation != "download" {
				t.Fatalf("operation = %q", operation)
			}
			if remote != "origin" {
				t.Fatalf("remote = %q", remote)
			}
			return backend, nil
		},
	}
	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var responses []response
	decoder := json.NewDecoder(&output)
	for decoder.More() {
		var item response
		if err := decoder.Decode(&item); err != nil {
			t.Fatal(err)
		}
		responses = append(responses, item)
	}
	return responses
}

func completeResponse(t *testing.T, responses []response, oid string) response {
	t.Helper()
	for _, item := range responses {
		if item.Event == "complete" && item.OID == oid {
			return item
		}
	}
	t.Fatalf("no completion for %s in %#v", oid, responses)
	return response{}
}

func TestAgentUploadAndIdempotentDuplicate(t *testing.T) {
	body := []byte("large-file-payload")
	oid := digest(body)
	path := t.TempDir() + "/payload"
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	memory := store.NewMemory()
	input := `{"event":"init","operation":"upload","remote":"origin","concurrent":true,"concurrenttransfers":8}` + "\n" +
		`{"event":"upload","oid":"` + oid + `","size":18,"path":"` + path + `","action":null}` + "\n" +
		`{"event":"terminate"}` + "\n"
	responses := runAgent(t, input, Backend{Store: seekRequiredStore{memory}, DownloadChunkSize: 3})
	if got := completeResponse(t, responses, oid); got.Error != nil || got.Path != "" {
		t.Fatalf("completion = %#v", got)
	}
	key := ".git/git3/lfs/objects/" + oid[:2] + "/" + oid[2:4] + "/" + oid
	object, err := memory.Get(context.Background(), key, "")
	if err != nil || !bytes.Equal(object.Body, body) {
		t.Fatalf("stored object = %q, %v", object.Body, err)
	}

	responses = runAgent(t, input, Backend{Store: memory, DownloadChunkSize: 3})
	if got := completeResponse(t, responses, oid); got.Error != nil {
		t.Fatalf("duplicate completion = %#v", got)
	}
}

func TestAgentDownloadReturnsVerifiedTemporaryFile(t *testing.T) {
	body := []byte("downloaded-lfs-object")
	oid := digest(body)
	key := objectKey(oid)
	memory := store.NewMemory()
	memory.Set(key, body)
	input := `{"event":"init","operation":"download","remote":"origin","concurrent":true,"concurrenttransfers":8}` + "\n" +
		`{"event":"download","oid":"` + oid + `","size":21,"action":null}` + "\n" +
		`{"event":"terminate"}` + "\n"
	responses := runAgent(t, input, Backend{Store: memory, DownloadChunkSize: 4, DownloadConcurrency: 2})
	got := completeResponse(t, responses, oid)
	if got.Error != nil || got.Path == "" {
		t.Fatalf("completion = %#v", got)
	}
	downloaded, err := os.ReadFile(got.Path)
	if err != nil || !bytes.Equal(downloaded, body) {
		t.Fatalf("download = %q, %v", downloaded, err)
	}
	_ = os.Remove(got.Path)
	lastProgress := int64(0)
	for _, item := range responses {
		if item.Event == "progress" {
			if item.BytesSoFar < lastProgress {
				t.Fatalf("progress moved backwards: %d after %d", item.BytesSoFar, lastProgress)
			}
			lastProgress = item.BytesSoFar
		}
	}
	if lastProgress != int64(len(body)) {
		t.Fatalf("final progress = %d", lastProgress)
	}
}

func TestAgentReportsObjectErrorsAndContinues(t *testing.T) {
	body := []byte("available")
	oid := digest(body)
	memory := store.NewMemory()
	memory.Set(objectKey(oid), body)
	missing := strings.Repeat("a", 64)
	input := `{"event":"init","operation":"download","remote":"origin"}` + "\n" +
		`{"event":"download","oid":"` + missing + `","size":1,"action":null}` + "\n" +
		`{"event":"download","oid":"` + oid + `","size":9,"action":null}` + "\n" +
		`{"event":"terminate"}` + "\n"
	responses := runAgent(t, input, Backend{Store: memory, DownloadChunkSize: 2})
	if got := completeResponse(t, responses, missing); got.Error == nil {
		t.Fatalf("missing completion = %#v", got)
	}
	got := completeResponse(t, responses, oid)
	if got.Error != nil || got.Path == "" {
		t.Fatalf("successful completion = %#v", got)
	}
	_ = os.Remove(got.Path)
}

func TestAgentRejectsCorruptDownloadAndRemovesTemporaryFile(t *testing.T) {
	expected := []byte("good-payload")
	oid := digest(expected)
	memory := store.NewMemory()
	memory.Set(objectKey(oid), []byte("bad--payload"))
	tempDir := t.TempDir()
	input := `{"event":"init","operation":"download","remote":"origin"}` + "\n" +
		`{"event":"download","oid":"` + oid + `","size":12,"action":null}` + "\n" +
		`{"event":"terminate"}` + "\n"
	responses := runAgentInTemp(t, input, Backend{Store: memory, DownloadChunkSize: 3}, tempDir)
	if got := completeResponse(t, responses, oid); got.Error == nil || got.Path != "" {
		t.Fatalf("completion = %#v", got)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed download left temporary files: %#v", entries)
	}
}

func TestAgentRejectsCorruptUploadBeforeWriting(t *testing.T) {
	body := []byte("payload")
	path := t.TempDir() + "/payload"
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("0", 64)
	memory := store.NewMemory()
	input := `{"event":"init","operation":"upload","remote":"origin"}` + "\n" +
		`{"event":"upload","oid":"` + oid + `","size":7,"path":"` + path + `","action":null}` + "\n" +
		`{"event":"terminate"}` + "\n"
	responses := runAgent(t, input, Backend{Store: memory})
	if got := completeResponse(t, responses, oid); got.Error == nil {
		t.Fatalf("completion = %#v", got)
	}
	if _, err := memory.Head(context.Background(), objectKey(oid)); err != store.ErrNotFound {
		t.Fatalf("corrupt upload stored: %v", err)
	}
}

func TestAgentRejectsMalformedProtocol(t *testing.T) {
	agent := Agent{In: strings.NewReader("not-json\n"), Out: new(bytes.Buffer), Open: func(context.Context, string, string) (Backend, error) {
		return Backend{}, nil
	}}
	if err := agent.Run(context.Background()); err == nil {
		t.Fatal("malformed init succeeded")
	}
}

func TestAgentReportsInitError(t *testing.T) {
	var output bytes.Buffer
	agent := Agent{
		In:  strings.NewReader("{\"event\":\"init\",\"operation\":\"download\",\"remote\":\"origin\"}\n"),
		Out: &output,
		Open: func(context.Context, string, string) (Backend, error) {
			return Backend{}, fmt.Errorf("remote unavailable")
		},
	}
	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || !strings.Contains(got.Error.Message, "remote unavailable") {
		t.Fatalf("init response = %#v", got)
	}
}
