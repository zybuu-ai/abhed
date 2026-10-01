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

// A -p stream-json capture leaves out agent.delta by design, and says so on its
// result line: a gap only deltas could fill is reported as that, not as a hole.
// A missing event is still critical, and so is a gap HawkEYE cannot account for.
func TestHawkeyeStreamJSONCapture(t *testing.T) {
	g := newSessRig(t)
	run := func(args ...string) []byte {
		t.Helper()
		out, err := g.cmd(append([]string{"-p", "write a.txt=hi", "-permission-mode", "acceptEdits"}, args...)...).Output()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	check := func(name string, data []byte, wantCode int, want ...string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name+".jsonl")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		rep, code := hawkeyeOn(t, path)
		if code != wantCode {
			t.Fatalf("%s: exit %d, want %d\n%s", name, code, wantCode, rep)
		}
		for _, w := range want {
			if !strings.Contains(rep, w) {
				t.Fatalf("%s: the report lacks %q\n%s", name, w, rep)
			}
		}
		return rep
	}
	lines := func(data []byte) []string { return strings.Split(strings.TrimSpace(string(data)), "\n") }

	stream := run("-output-format", "stream-json")
	if !strings.Contains(string(stream), `"omitted":["agent.delta","agent.reasoning.delta"]`) {
		t.Fatalf("the result line does not say what stream-json left out:\n%s", stream)
	}
	events, _ := parseEvents(stream)
	gaps := 0
	for i := 1; i < len(events); i++ {
		if events[i].Seq != events[i-1].Seq+1 {
			gaps++
		}
	}
	if gaps == 0 {
		t.Fatalf("the capture has no gap, so it tests nothing:\n%s", stream)
	}
	if rep := check("stream", stream, 0, "agent.delta omitted by stream-json", "no gaps"); strings.Contains(rep, "Events are missing") {
		t.Fatalf("omitted deltas reported as missing events:\n%s", rep)
	}

	// The same capture with one non-delta event taken out: each is a real hole.
	for _, drop := range []agent.EventType{agent.EvObservation, agent.EvModelCall, agent.EvUserMessage, agent.EvAgentMessage} {
		var kept []string
		dropped := false
		for _, l := range lines(stream) {
			var ev agent.Event
			if !dropped && json.Unmarshal([]byte(l), &ev) == nil && ev.Type == drop {
				dropped = true
				continue
			}
			kept = append(kept, l)
		}
		if !dropped {
			t.Fatalf("the capture has no %s to remove:\n%s", drop, stream)
		}
		check("without-"+string(drop), []byte(strings.Join(kept, "\n")+"\n"), 3, "Events are missing from the record")
	}

	// A result line that names nothing omitted: HawkEYE cannot tell, says so, and fails closed.
	silent := strings.Replace(string(stream), `,"omitted":["agent.delta","agent.reasoning.delta"]`, "", 1)
	check("unsaid", []byte(silent), 3, "cannot tell omitted deltas from missing events")

	// json and stream-json with the fragments leave nothing out, and still pass.
	check("json", run("-output-format", "json"), 0, "no gaps")
	partial := run("-output-format", "stream-json", "-include-partial-messages")
	if strings.Contains(string(partial), `"omitted"`) {
		t.Fatalf("a capture with the fragments says it omitted them:\n%s", partial)
	}
	check("partial", partial, 0, "no gaps")
}
