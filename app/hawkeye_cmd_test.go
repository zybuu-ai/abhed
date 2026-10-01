package app

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
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
	code := hawkeyeCmd(t.TempDir(), []string{path}, "")
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

// hawkeyeIn runs abhed hawkeye on target in workspace ws.
func hawkeyeIn(t *testing.T, ws, target string) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := hawkeyeCmd(ws, []string{target}, config.TrustGranted)
	os.Stdout = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out), code
}

// HawkEYE finds a session by id in the local record, and reads an export of
// it, checked against the head its last line carries; a changed copy fails.
func TestHawkeyeReadsTheLocalRecord(t *testing.T) {
	g := newSessRig(t)
	c := g.start()
	g.ask(c, "Remember the codeword ZEBRA-41.")
	c.command("/export s.jsonl", "wrote ")
	exit(c)
	id := g.sessions()[0].ID
	t.Setenv("HOME", g.home)

	out, code := hawkeyeIn(t, g.ws, id)
	if code == 1 || !strings.Contains(out, "HawkEYE · "+id) || !strings.Contains(out, "record: verified") {
		t.Fatalf("by id: exit %d\n%s", code, out)
	}
	export := filepath.Join(g.ws, "s.jsonl")
	out, code = hawkeyeIn(t, g.ws, export)
	if code == 1 || !strings.Contains(out, "HawkEYE · "+id) || !strings.Contains(out, "record: verified against its head") {
		t.Fatalf("export: exit %d\n%s", code, out)
	}
	data, _ := os.ReadFile(export)
	tampered := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(tampered, []byte(strings.Replace(string(data), "ZEBRA-41", "ZEBRA-42", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code = hawkeyeIn(t, g.ws, tampered); code != 3 || !strings.Contains(out, "FAILED verification") {
		t.Fatalf("a changed export: exit %d\n%s", code, out)
	}
}

// HawkEYE reads a -p run's own json and stream-json output: the result line
// at the end says how the run ended and is no event.
func TestHawkeyeSkipsTheResultLine(t *testing.T) {
	g := newSessRig(t)
	for _, format := range []string{"json", "stream-json"} {
		out, err := g.cmd("-p", "hello", "-output-format", format).Output()
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if !strings.Contains(string(out), `"type":"result"`) {
			t.Fatalf("%s: no result line:\n%s", format, out)
		}
		events, err := parseEvents(out)
		if err != nil || len(events) == 0 {
			t.Fatalf("%s: %v", format, err)
		}
		for _, ev := range events {
			if ev.Type == "result" || ev.Seq == 0 {
				t.Fatalf("%s: the result line was read as an event: %+v", format, ev)
			}
		}
		path := filepath.Join(t.TempDir(), "run.jsonl")
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Fatal(err)
		}
		if rep, code := hawkeyeOn(t, path); code == 1 || strings.Contains(rep, "153722867") {
			t.Fatalf("%s: exit %d\n%s", format, code, rep)
		}
	}
}
