package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// breakingRedactor stands in for a secrets store that stopped loading: every
// payload comes back unreadable, so the record withholds it.
type breakingRedactor struct{}

func (breakingRedactor) Redact([]byte) []byte { return []byte("not json") }
func (breakingRedactor) Span() int            { return 0 }

// An ask whose record was withheld is refused by the system, never put to an
// approver who would be shown nothing to decide on.
func TestWithheldAskIsDeniedNotPut(t *testing.T) {
	dir := tempDir(t)
	l, store := harnessIn(t, dir, writeTurns(dir), policy.ModeDefault, true)
	l.Recorder.Redact = breakingRedactor{}
	asked := 0
	l.Approver = approverFunc(func(context.Context, policy.Result) (bool, error) { asked++; return true, nil })
	if _, err := l.Run(context.Background(), "write it"); err != nil {
		t.Fatal(err)
	}
	if asked != 0 {
		t.Fatalf("approver was asked %d times about a withheld request", asked)
	}
	evs, _ := store.Events(l.Recorder.sessionID)
	denied := 0
	for _, e := range evs {
		switch {
		case e.Type == EvActionApproved:
			t.Fatalf("a withheld ask was approved: %s", e.Payload)
		case e.Type == EvActionDenied && e.Actor == ActorSystem:
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("%d system denials, want 1", denied)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err == nil {
		t.Fatal("the write ran")
	}
}
