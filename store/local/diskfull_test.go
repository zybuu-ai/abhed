package local

import (
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A write that stops part way, as on a full disk, is undone: the record
// verifies, and the next append follows the last whole line.
func TestPartialWriteIsUndone(t *testing.T) {
	s := openTest(t, t.TempDir())
	rec := record(t, s, "s-1", "one")
	old := writeLine
	writeLine = func(f *os.File, b []byte) (int, error) {
		n, _ := f.Write(b[:len(b)/2])
		return n, syscall.ENOSPC
	}
	_, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "two"})
	writeLine = old
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("the failed write: %v", err)
	}
	mustVerify(t, s, "s-1")
	if _, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "three"}); err != nil {
		t.Fatal(err)
	}
	if rep := mustVerify(t, s, "s-1"); rep.Events != 2 {
		t.Fatalf("%d events after the failed write", rep.Events)
	}
}
