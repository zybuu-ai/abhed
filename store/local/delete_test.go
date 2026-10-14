package local

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A session refused right after it was recorded, still held by this process,
// is deleted: its file and its subagents go, a tombstone stays, nothing lists
// it, and the record still verifies. Deleting it again is no error.
func TestDeleteSessionErasesAHeldSession(t *testing.T) {
	s := openTest(t, t.TempDir())
	rec := record(t, s, "s-del", "the launch codes are 0000")
	sha, _ := s.Blobs().Put([]byte("a pre-image only s-del names"))
	_, _ = rec.Record(agent.EvCheckpoint, agent.ActorSystem, agent.Trusted, agent.CheckpointSaved{Path: "a", SHA256: sha})
	_ = s.CreateSubagentSession(context.Background(), "s-del-child", "s-del", "sub")
	if !s.Held("s-del") {
		t.Fatal("the test needs the session held, as a refused start leaves it")
	}
	var d agent.SessionDeleter = s
	if err := d.DeleteSession("s-del"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.Path("s-del"), s.Path("s-del-child")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind", p)
		}
	}
	if _, err := s.Blobs().Get(sha); !errors.Is(err, ErrNotFound) {
		t.Fatal("its pre-image was kept")
	}
	if e, _ := s.index.get("s-del"); !e.Pruned {
		t.Fatal("no tombstone")
	}
	if l, _ := s.Index().List(Filter{All: true}); len(l) != 0 {
		t.Fatalf("a deleted session is still listed: %+v", l)
	}
	if rep, err := s.VerifyIndex(); err != nil || !rep.OK {
		t.Fatalf("the index after a delete: %+v %v", rep, err)
	}
	if err := d.DeleteSession("s-del"); err != nil {
		t.Fatalf("deleting it again: %v", err)
	}
	if err := d.DeleteSession("s-never"); err != nil {
		t.Fatalf("deleting one never recorded: %v", err)
	}
}
