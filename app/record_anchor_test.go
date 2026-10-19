package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/witness"
)

// A registered anchor is given the local record's heads beside the witness,
// and stopped when the record closes; one that fails to build stops the open.
func TestRecordAnchorGetsTheLocalHeads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saved := recordAnchors
	t.Cleanup(func() { recordAnchors = saved })
	var mu sync.Mutex
	var heads []RecordHead
	stopped := 0
	recordAnchors = []AnchorBuilder{func(config.Config) (func(RecordHead), func(), error) {
		return func(h RecordHead) { mu.Lock(); heads = append(heads, h); mu.Unlock() }, func() { stopped++ }, nil
	}}
	wfile := filepath.Join(t.TempDir(), "heads.jsonl")
	cfg := config.Config{Record: config.RecordConfig{Witness: config.WitnessConfig{File: wfile}}, ManagedKeys: []string{"record.witness"}}
	rec, err := openRecord(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id := newConversationID()
	if err := rec.CreateSession(context.Background(), store.SessionRecord{ID: id, Workspace: t.TempDir(), User: cliUser()}); err != nil {
		t.Fatal(err)
	}
	r := agent.NewRecorder(rec, id, "")
	_, _ = r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "hi"})
	_, _ = r.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted, Turns: 1})
	_ = rec.Close()
	mu.Lock()
	defer mu.Unlock()
	var last RecordHead
	for _, h := range heads {
		if h.Session == id {
			last = h
		}
	}
	if last.Store != "local" || last.Tenant != "default" || last.Lines != 2 || len(last.Hash) != 64 || stopped != 1 {
		t.Fatalf("heads %+v, stopped %d", heads, stopped)
	}
	// The witness still had them too.
	f, err := os.Open(wfile)
	if err != nil {
		t.Fatal(err)
	}
	stmts, _ := witness.Read(f)
	_ = f.Close()
	var held int64
	for _, st := range stmts {
		if st.Session == id {
			held = st.Lines
		}
	}
	if held != 2 {
		t.Fatalf("the witness lost its heads to the added anchor: %+v", stmts)
	}
	recordAnchors = []AnchorBuilder{func(config.Config) (func(RecordHead), func(), error) {
		return nil, nil, errors.New("key unavailable")
	}}
	if _, err := openRecord(config.Config{}); err == nil || !strings.Contains(err.Error(), "key unavailable") {
		t.Fatalf("an anchor that failed to build: %v", err)
	}
	// None registered and no witness: no anchor at all.
	recordAnchors = nil
	if a, s, err := recordAnchor(config.Config{}); a != nil || s != nil || err != nil {
		t.Fatal("an anchor was made from nothing")
	}
}
