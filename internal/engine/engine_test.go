package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sync"
	"testing"

	"github.com/robertpitt/git3/internal/canonical"
	"github.com/robertpitt/git3/internal/model"
	"github.com/robertpitt/git3/internal/store"
)

type recordingProgress struct {
	mu     sync.Mutex
	native bytes.Buffer
	events []ProgressEvent
}

func (p *recordingProgress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.native.Write(data)
}

func (p *recordingProgress) Update(event ProgressEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
}

func (p *recordingProgress) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.native.String()
}

func (p *recordingProgress) completed(phase string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, event := range p.events {
		if event.Phase == phase && event.Done {
			return true
		}
	}
	return false
}

func (p *recordingProgress) empty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.native.Len() == 0 && len(p.events) == 0
}

func TestTrailerWriterKeepsOnlyBoundedTail(t *testing.T) {
	hash := sha256.New()
	w := &trailerWriter{h: hash, want: 20}
	input := make([]byte, 1<<20)
	for i := range input {
		input[i] = byte(i)
	}
	if n, err := w.Write(input); err != nil || n != len(input) {
		t.Fatalf("write = %d, %v", n, err)
	}
	if len(w.tail) != w.want || cap(w.tail) != w.want {
		t.Fatalf("tail len=%d cap=%d want=%d", len(w.tail), cap(w.tail), w.want)
	}
	want := input[len(input)-w.want:]
	for i := range want {
		if w.tail[i] != want[i] {
			t.Fatalf("tail differs at byte %d", i)
		}
	}
}

func TestAdminConsumersReuseResolvedLogGraph(t *testing.T) {
	ctx := context.Background()
	const (
		repositoryID = "00000000-0000-4000-8000-000000000001"
		packsetID    = "00000000-0000-4000-8000-000000000002"
		pageID       = "00000000-0000-4000-8000-000000000003"
	)
	packset := model.Packset{FormatVersion: 1, RepositoryID: repositoryID, ObjectFormat: "sha1", PacksetID: packsetID, Levels: []model.PackLevel{}}
	packsetBytes, err := canonical.Marshal(packset)
	if err != nil {
		t.Fatal(err)
	}
	packsetRef := shaRef(".git/git3/packsets/"+packsetID+".json", packsetBytes)
	tx := model.Transaction{RepositoryID: repositoryID}
	descriptorBytes, err := canonical.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	descriptorRef := shaRef(".git/git3/transactions/00000000000000000001-"+pageID+".json", descriptorBytes)
	pageRef := shaRef(".git/git3/log-pages/"+pageID+".json", []byte("must not be read again"))
	mem := store.NewMemory()
	mem.Set(packsetRef.Key, packsetBytes)
	mem.Set(descriptorRef.Key, descriptorBytes)
	repo := &Repository{Store: mem}
	state := &RemoteState{
		Head: model.Head{
			RepositoryID: repositoryID,
			ObjectFormat: "sha1",
			Packset:      model.PacksetPointer{PacksetID: packsetID, Object: packsetRef},
			RefSnapshot:  model.SnapshotPointer{Object: shaRef(".git/git3/snapshots/unused.refs", nil)},
			Log:          model.Log{TipPage: &model.PagePointer{PageID: pageID, Object: pageRef}},
		},
		logResolved:  true,
		logEnvelopes: []model.Envelope{{Descriptor: descriptorRef, Transaction: tx}},
		logPages:     []model.ObjectRef{pageRef},
	}
	if err = repo.verifyDescriptorGraph(ctx, state); err != nil {
		t.Fatal(err)
	}
	live, err := repo.liveKeys(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if !live[pageRef.Key] {
		t.Fatal("resolved log page was not retained in the live set")
	}
	for _, request := range mem.Requests {
		if request == "GET "+pageRef.Key {
			t.Fatalf("admin consumer re-read validated log page: %v", mem.Requests)
		}
	}
}
