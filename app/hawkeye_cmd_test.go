package app

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// hawkeyeOn runs abhed hawkeye on a file and returns its stdout and exit code.
func hawkeyeOn(t *testing.T, path string) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := hawkeyeCmd(t.TempDir(), []string{path})
	os.Stdout = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out), code
}

// HawkEYE reads the stream -p -output-format json writes, one event per
// line, as well as the array /export writes, and reports the same on both.
func TestHawkeyeReadsJSONLinesAndExports(t *testing.T) {
	at := time.Now().UTC()
	msg, _ := json.Marshal(agent.Message{Text: "go"})
	end, _ := json.Marshal(agent.SessionEnded{Reason: agent.TermCompleted, Turns: 1})
	events := []agent.Event{
		{ID: "e1", SessionID: "s-jsonl", Seq: 1, Type: agent.EvUserMessage, Actor: agent.ActorUser, Payload: msg, CreatedAt: at},
		{ID: "e2", SessionID: "s-jsonl", Seq: 2, Type: agent.EvSessionEnded, Actor: agent.ActorSystem, Payload: end, CreatedAt: at},
	}
	dir := t.TempDir()
	var lines []string
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		lines = append(lines, string(b))
	}
	jsonl := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(jsonl, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	array := filepath.Join(dir, "session.json")
	b, _ := json.MarshalIndent(events, "", "  ")
	if err := os.WriteFile(array, b, 0o600); err != nil {
		t.Fatal(err)
	}

	fromLines, code := hawkeyeOn(t, jsonl)
	if code == 1 || !strings.Contains(fromLines, "HawkEYE · s-jsonl") {
		t.Fatalf("JSON lines: exit %d\n%s", code, fromLines)
	}
	fromArray, _ := hawkeyeOn(t, array)
	if fromArray != fromLines {
		t.Fatalf("the two forms report differently:\n%s\n---\n%s", fromArray, fromLines)
	}

	junk := filepath.Join(dir, "junk.jsonl")
	if err := os.WriteFile(junk, []byte(`{"hello":"world"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, code := hawkeyeOn(t, junk); code != 1 {
		t.Fatalf("lines that are not events: exit %d, want 1", code)
	}
}
