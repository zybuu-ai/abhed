package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/hawkeye"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/termline"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// waitOutput waits until a terminal's output from index from on holds want.
func waitOutput(cl *studioClient, from int, id, want string) {
	cl.t.Helper()
	var got strings.Builder
	cl.waitFor(from, "terminal output "+want, func(m rpcMessage) bool {
		got.WriteString(terminalOutput([]rpcMessage{m}, id))
		return strings.Contains(got.String(), want)
	})
}

// terminalOutput joins what an Abhed terminal sent its view.
func terminalOutput(msgs []rpcMessage, id string) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Method != "_abhed/terminal/output" {
			continue
		}
		var p struct {
			TerminalID string `json:"terminalId"`
			Data       string `json:"data"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.TerminalID == id {
			raw, _ := base64.StdEncoding.DecodeString(p.Data)
			b.Write(raw)
		}
	}
	return b.String()
}

// §7.1: a line in the Abhed terminal is the person's bash call: put to
// policy, run in the session's sandbox, and recorded by: user; a destructive
// one runs only after the person confirms, and not when they decline.
func TestStudioTerminalLines(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"deny":["bash(curl *)"]}`)
	id := r.open()
	var term struct {
		TerminalID string `json:"terminalId"`
		Tier       string `json:"tier"`
		Recorded   bool   `json:"recorded"`
	}
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "lines", "cols": 80, "rows": 24}, &term)
	if term.TerminalID == "" || term.Tier == "" || term.Tier == "none" || !term.Recorded {
		t.Fatalf("create: %+v", term)
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "echo studio-$((40+2))\r"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "studio-42\r\n")
	reqs, actors := r.recorded(id, agent.EvActionRequested)
	if len(reqs) != 1 || actors[0] != agent.ActorUser || !strings.Contains(reqs[0]["args"].(map[string]any)["command"].(string), "echo studio-") {
		t.Fatalf("action.requested: %v %v", reqs, actors)
	}
	// A deny rule holds for the person as for the agent.
	from = r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "curl example.com\n"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "Denied")
	// A destructive line asks Studio's modal; a decline runs nothing.
	victim := r.write("victim/keep.txt", "keep")
	var confirms int
	r.cl.answering(func(method string, params json.RawMessage) any {
		if method == "_abhed/terminal/confirm" {
			confirms++
			return map[string]any{"confirmed": false}
		}
		return map[string]any{}
	})
	from = r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "rm -rf " + filepath.Dir(victim) + "\n"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "not confirmed")
	if confirms != 1 {
		t.Fatalf("confirms: %d", confirms)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("a declined destructive command ran")
	}
	from = r.cl.mark()
	r.cl.ok("_abhed/terminal/kill", map[string]any{"terminalId": term.TerminalID}, nil)
	r.cl.waitFor(from, "terminal exit", func(m rpcMessage) bool { return m.Method == "_abhed/terminal/exit" })
	r.cl.refused(errParams, "_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "ls\n"})
}

// A lines-mode terminal reads like a shell: a prompt naming where the next
// line runs, the typed command in bold, its output, then its exit status.
func TestStudioTerminalLinesShowOutputBeforeTheStatus(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	r.write("sub/keep.txt", "")
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "lines", "cols": 80, "rows": 24}, &term)
	waitOutput(r.cl, from, term.TerminalID, " $\x1b[0m ")
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "echo order-check\r"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "\r\norder-check\r\n")
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "cd sub\r"}, nil)
	waitOutput(r.cl, from, term.TerminalID, filepath.Join(filepath.Base(r.ws), "sub")+" $")
	got := terminalOutput(r.cl.since(from), term.TerminalID)
	root := "\x1b[2m" + filepath.Base(r.ws) + " $\x1b[0m "
	want := root + "\x1b[1mecho order-check\x1b[0m\r\norder-check\r\n\x1b[2mexit 0 · "
	if !strings.HasPrefix(got, want) {
		t.Fatalf("the line's output, status and prompt are out of order:\n%q\nwant prefix\n%q", got, want)
	}
	if strings.Contains(got, "[no output]") {
		t.Fatalf("the record's placeholder reached the terminal: %q", got)
	}
	if !strings.HasSuffix(got, "ms\x1b[0m\r\n\x1b[2m"+filepath.Join(filepath.Base(r.ws), "sub")+" $\x1b[0m ") {
		t.Fatalf("the prompt does not follow the cd: %q", got)
	}
	// A cd that is not followed has no output but a note: the note shows, the placeholder does not.
	at := r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "cd /\r"}, nil)
	waitOutput(r.cl, at, term.TerminalID, "outside the workspace")
	if got := terminalOutput(r.cl.since(at), term.TerminalID); strings.Contains(got, "[no output]") {
		t.Fatalf("the placeholder reached the terminal after a refused cd: %q", got)
	}
}

// Keys that send escape sequences, an arrow or a bracketed paste's markers,
// are dropped whole in lines mode; none leaves its tail to run as a command.
func TestStudioTerminalLinesDropEscapeSequences(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "lines", "cols": 80, "rows": 24}, &term)
	waitOutput(r.cl, from, term.TerminalID, " $\x1b[0m ")
	for _, in := range []string{
		"\x1b[A\r",                              // an arrow alone
		"\x1b[Aecho one\x1bOB\r",                // CSI and SS3 arrows around a line
		"\x1b[200~echo two\x1b[201~\r",          // a bracketed paste
		"\x1b]0;title\x07\x1b[1;5Decho three\r", // a terminal's reply and Ctrl-Left
		"\x1b[1;5", "Cecho four\r",              // a sequence split across inputs
		"\x1b", "echo five\r", // the Esc key on its own
	} {
		r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": in}, nil)
	}
	waitOutput(r.cl, from, term.TerminalID, "\r\nfive\r\n")
	var ran []string
	reqs, _ := r.recorded(id, agent.EvActionRequested)
	for _, q := range reqs {
		ran = append(ran, q["args"].(map[string]any)["command"].(string))
	}
	if want := []string{"echo one", "echo two", "echo three", "echo four", "echo five"}; !slices.Equal(ran, want) {
		t.Fatalf("lines run: %q, want %q", ran, want)
	}
	if out := terminalOutput(r.cl.since(from), term.TerminalID); strings.Contains(out, "not found") {
		t.Fatalf("part of a key sequence ran: %q", out)
	}
}

// A Tab typed or pasted into a line separates words as bash does, so the
// line judged and run is the one bash would see, not two words joined.
func TestStudioTerminalLinesTabSeparatesWords(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"deny":["bash(wget *)"]}`)
	id := r.open()
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "lines", "cols": 80, "rows": 24}, &term)
	waitOutput(r.cl, from, term.TerminalID, " $\x1b[0m ")
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "wget\thttp://x\r"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "Denied")
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "echo\ttab-ok\r"}, nil)
	waitOutput(r.cl, from, term.TerminalID, "tab-ok\r\n\x1b[2mexit 0")
	var ran []string
	reqs, _ := r.recorded(id, agent.EvActionRequested)
	for _, q := range reqs {
		ran = append(ran, q["args"].(map[string]any)["command"].(string))
	}
	if want := []string{"wget http://x", "echo tab-ok"}; !slices.Equal(ran, want) {
		t.Fatalf("lines judged: %q, want %q", ran, want)
	}
}

// Ctrl-C while a line runs stops it and drops the lines typed ahead of the
// prompt, as a shell flushes its input on an interrupt, and says so.
func TestStudioTerminalLinesCtrlCDropsTypedAhead(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "lines", "cols": 80, "rows": 24}, &term)
	waitOutput(r.cl, from, term.TerminalID, " $\x1b[0m ")
	input := func(data string) {
		r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": data}, nil)
	}
	input("sleep 30\r")
	r.cl.conn.termMu.Lock()
	at := r.cl.conn.terms[term.TerminalID]
	r.cl.conn.termMu.Unlock()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		at.mu.Lock()
		running := at.running != nil
		at.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the line never started")
		}
	}
	input("echo typed-ahead\r")
	input("\x03")
	waitOutput(r.cl, from, term.TerminalID, "abhed: 1 typed-ahead line was not run\r\n")
	input("echo after\r")
	waitOutput(r.cl, from, term.TerminalID, "\r\nafter\r\n")
	var ran []string
	reqs, _ := r.recorded(id, agent.EvActionRequested)
	for _, q := range reqs {
		ran = append(ran, q["args"].(map[string]any)["command"].(string))
	}
	if want := []string{"sleep 30", "echo after"}; !slices.Equal(ran, want) {
		t.Fatalf("lines run: %q, want %q", ran, want)
	}
}

// A session let go of, by session/close or the editor going away, records
// its end, after its terminals' last lines, so its report has one; one a
// run ended keeps that run's totals.
func TestStudioSessionRecordsItsEndWhenLetGo(t *testing.T) {
	r := newStudioRig(t, "")
	lines := func(id, data, want string) {
		var term struct {
			TerminalID string `json:"terminalId"`
		}
		from := r.cl.mark()
		r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "lines", "cols": 80, "rows": 24}, &term)
		r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": data}, nil)
		waitOutput(r.cl, from, term.TerminalID, want)
	}
	lastEnd := func(id string) (agent.SessionEnded, []agent.Event) {
		t.Helper()
		evs := r.events(id)
		last := evs[len(evs)-1]
		var end agent.SessionEnded
		if last.Type != agent.EvSessionEnded || json.Unmarshal(last.Payload, &end) != nil {
			t.Fatalf("the record ends with %s, not session.ended", last.Type)
		}
		if rep := hawkeye.Analyze(id, evs); slices.ContainsFunc(rep.Findings, func(f hawkeye.Finding) bool { return f.Code == "no-end" }) {
			t.Fatalf("HawkEYE finds no end: %+v", rep.Findings)
		}
		return end, evs
	}

	closed := r.open()
	lines(closed, "echo closed\r", "\r\nclosed\r\n")
	r.cl.ok("session/close", map[string]any{"sessionId": closed}, nil)
	if end, _ := lastEnd(closed); end.Reason != agent.TermSessionClosed {
		t.Fatalf("session/close ended it as %q", end.Reason)
	}

	ran := r.open()
	if stop, _ := r.prompt(ran, "hello"); stop != "end_turn" {
		t.Fatalf("stop: %s", stop)
	}
	lines(ran, "sleep 30\r", "\x1b[1msleep 30\x1b[0m\r\n")
	left := r.open()
	// The editor going away lets every session go.
	r.cl.conn.closeAll()
	end, evs := lastEnd(ran)
	if end.Reason != agent.TermSessionClosed || end.Turns != 1 || end.TokensIn == 0 {
		t.Fatalf("closing end after a run: %+v", end)
	}
	if obs := evs[len(evs)-2]; obs.Type != agent.EvObservation {
		t.Fatalf("the stopped line's result is not before the end: %s", obs.Type)
	}
	if end, _ := lastEnd(left); end.Reason != agent.TermSessionClosed {
		t.Fatalf("an untouched session ended as %q", end.Reason)
	}
	// The run's end and the closing one; a session closed before records no second.
	if a, b := count(r.events(ran), agent.EvSessionEnded), count(r.events(closed), agent.EvSessionEnded); a != 2 || b != 1 {
		t.Fatalf("ends: %d after a run, %d once closed", a, b)
	}
}

func TestLineResultPutsTheStatusLast(t *testing.T) {
	code := 1
	got := string(lineResult(tools.Result{Content: "exit 1 · 12ms\nboom\nbang", ExitCode: &code}))
	if got != "boom\r\nbang\r\n\x1b[2mexit 1 · 12ms\x1b[0m\r\n" {
		t.Fatalf("%q", got)
	}
	got = string(lineResult(tools.Result{Content: "exit 1 · 12ms\n[no output]", ExitCode: &code, NoOutput: true}))
	if got != "\x1b[2mexit 1 · 12ms\x1b[0m\r\n" {
		t.Fatalf("no output: %q", got)
	}
	// The placeholder goes, the tool's note after it stays.
	zero := 0
	got = string(lineResult(tools.Result{Content: "exit 0 · 3ms\n[no output]\n\ncd: / is outside the workspace; still in .", ExitCode: &zero, NoOutput: true}))
	if got != "cd: / is outside the workspace; still in .\r\n\x1b[2mexit 0 · 3ms\x1b[0m\r\n" {
		t.Fatalf("note after no output: %q", got)
	}
	// Real output keeps its leading blank lines, and real text that reads like the placeholder.
	got = string(lineResult(tools.Result{Content: "exit 0 · 3ms\n\n\nafter", ExitCode: &zero}))
	if !strings.HasPrefix(got, "\r\n\r\nafter") {
		t.Fatalf("leading blank lines dropped: %q", got)
	}
	got = string(lineResult(tools.Result{Content: "exit 0 · 3ms\n[no output]", ExitCode: &zero}))
	if !strings.HasPrefix(got, "[no output]\r\n") {
		t.Fatalf("real placeholder text hidden: %q", got)
	}
	// A command whose real output is the placeholder's text keeps it.
	got = string(lineResult(tools.Result{Content: "exit 0 · 9ms\n[no output]\nreal\n", ExitCode: &code}))
	if !strings.HasPrefix(got, "[no output]\r\nreal") {
		t.Fatalf("real output hidden: %q", got)
	}
	// A refusal or a tool error has no status to move.
	if got := string(lineResult(tools.Result{Content: "Denied: curl", IsError: true})); got != "Denied: curl\r\n" {
		t.Fatalf("refusal: %q", got)
	}
}

// §7.3: a person's save is recorded manual.edit by: user; a path outside
// the session's roots is dropped, not recorded.
func TestStudioManualEdit(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	p := r.write("notes.md", "hello")
	sha := strings.Repeat("ab", 32)
	r.cl.ok("_abhed/manual/edited", map[string]any{"sessionId": id, "path": p, "beforeSha256": "", "afterSha256": sha, "patch": "+hello"}, nil)
	r.cl.ok("_abhed/manual/edited", map[string]any{"sessionId": id, "path": "/etc/hosts", "afterSha256": sha}, nil)
	r.cl.refused(errParams, "_abhed/manual/edited", map[string]any{"sessionId": id, "path": p, "afterSha256": "nothex"})
	edits, actors := r.recorded(id, agent.EvManualEdit)
	if len(edits) != 1 || edits[0]["by"] != "user" || actors[0] != agent.ActorUser || !strings.HasSuffix(edits[0]["path"].(string), "notes.md") {
		t.Fatalf("manual.edit: %v %v", edits, actors)
	}
}

// §7.4 and §7.5: the agent's change is reviewed hunk by hunk: an accepted
// hunk moves the baseline, a rejected one is written back as the person's
// write, and undo puts a turn's files back, each recorded by: user.
func TestStudioReviewAndUndo(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"mode":"accept-edits"}`)
	file := r.write("f.txt", "a\nb\nc\nd\ne\nf\ng\n")
	r.model.script(callTool("c1", "read", map[string]any{"path": file}),
		callTool("c2", "write", map[string]any{"path": file, "content": "A\nb\nc\nd\ne\nf\nG\n"}), say("done"))
	id := r.open()
	r.prompt(id, "change it")
	if r.read("f.txt") != "A\nb\nc\nd\ne\nf\nG\n" {
		t.Fatalf("the agent's write: %q", r.read("f.txt"))
	}
	var list struct {
		Files []map[string]any `json:"files"`
	}
	r.cl.ok("_abhed/review/list", map[string]any{"sessionId": id}, &list)
	if len(list.Files) != 1 || list.Files[0]["hunks"] != float64(2) || list.Files[0]["status"] != "modified" {
		t.Fatalf("review/list: %v", list.Files)
	}
	var base struct {
		Text string `json:"text"`
	}
	r.cl.ok("_abhed/review/baseline", map[string]any{"sessionId": id, "path": file}, &base)
	if base.Text != "a\nb\nc\nd\ne\nf\ng\n" {
		t.Fatalf("baseline %q", base.Text)
	}
	var left struct {
		Remaining int `json:"remaining"`
	}
	r.cl.ok("_abhed/review/accept", map[string]any{"sessionId": id, "path": file, "hunk": 0}, &left)
	if left.Remaining != 1 {
		t.Fatalf("after accepting one hunk: %d", left.Remaining)
	}
	r.cl.ok("_abhed/review/reject", map[string]any{"sessionId": id, "path": file, "hunk": 0}, &left)
	if left.Remaining != 0 || r.read("f.txt") != "A\nb\nc\nd\ne\nf\ng\n" {
		t.Fatalf("after rejecting the other: %d %q", left.Remaining, r.read("f.txt"))
	}
	acc, actors := r.recorded(id, agent.EvChangeAccepted)
	if len(acc) != 1 || acc[0]["by"] != "user" || actors[0] != agent.ActorUser {
		t.Fatalf("change.accepted: %v", acc)
	}
	writes, wactors := r.recorded(id, agent.EvActionRequested)
	last := len(writes) - 1
	if writes[last]["tool"] != "write" || wactors[last] != agent.ActorUser {
		t.Fatalf("the reject was not the person's write: %v %v", writes[last], wactors[last])
	}
	r.cl.refused(errParams, "_abhed/review/accept", map[string]any{"sessionId": id, "path": file, "hunk": 7})

	// Undo puts a later turn's file back to what it was before that turn.
	r.model.script(callTool("c3", "read", map[string]any{"path": file}),
		callTool("c4", "write", map[string]any{"path": file, "content": "rewritten\n"}), say("done"))
	r.prompt(id, "rewrite it")
	if r.read("f.txt") != "rewritten\n" {
		t.Fatalf("the second write: %q", r.read("f.txt"))
	}
	var undone struct {
		Restored []map[string]any `json:"restored"`
	}
	r.cl.ok("_abhed/review/undoTurn", map[string]any{"sessionId": id, "turn": 2}, &undone)
	if len(undone.Restored) != 1 || undone.Restored[0]["path"] != file || r.read("f.txt") != "A\nb\nc\nd\ne\nf\ng\n" {
		t.Fatalf("undo: %v %q", undone.Restored, r.read("f.txt"))
	}
	if restored, _ := r.recorded(id, agent.EvFileRestored); len(restored) != 1 || restored[0]["by"] != "user" {
		t.Fatalf("file.restored: %v", restored)
	}
	r.cl.refused(errParams, "_abhed/review/undoTurn", map[string]any{"sessionId": id, "turn": 9})
}

// Rejecting a file the agent made removes it as the person's delete, put to
// the delete rules and recorded like any action of theirs.
func TestStudioRejectOfANewFileIsTheirDelete(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"mode":"accept-edits","deny":["delete(**/vault/**)"]}`)
	kept, gone := filepath.Join(r.ws, "vault", "new.txt"), filepath.Join(r.ws, "new.txt")
	r.model.script(callTool("c1", "write", map[string]any{"path": kept, "content": "x\n"}),
		callTool("c2", "write", map[string]any{"path": gone, "content": "y\n"}), say("done"))
	id := r.open()
	r.prompt(id, "make them")
	r.cl.refused(errPolicy, "_abhed/review/reject", map[string]any{"sessionId": id, "path": kept})
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("a delete rule's file was removed: %v", err)
	}
	r.cl.ok("_abhed/review/reject", map[string]any{"sessionId": id, "path": gone}, nil)
	if _, err := os.Stat(gone); err == nil {
		t.Fatal("the rejected file is still there")
	}
	reqs, actors := r.recorded(id, agent.EvActionRequested)
	deletes := 0
	for i, q := range reqs {
		if q["tool"] == "delete" && actors[i] == agent.ActorUser {
			deletes++
		}
	}
	denied, _ := r.recorded(id, agent.EvActionDenied)
	if deletes != 2 || len(denied) != 1 || denied[0]["by"] != "policy" {
		t.Fatalf("deletes %d, denied %v", deletes, denied)
	}
}

// §7.6: steering needs a running prompt; the queue lists what waits.
func TestStudioSteerAndQueue(t *testing.T) {
	r := newStudioRig(t, "")
	id := r.open()
	r.cl.refused(errRefused, "_abhed/session/steer", map[string]any{"sessionId": id, "text": "faster"})
	var q struct {
		Items []any `json:"items"`
	}
	r.cl.ok("_abhed/queue/list", map[string]any{"sessionId": id}, &q)
	if len(q.Items) != 0 {
		t.Fatalf("queue: %v", q.Items)
	}
	r.cl.refused(errParams, "_abhed/queue/cancel", map[string]any{"sessionId": id, "id": "q_x"})
	// While a prompt runs, a steer waits in the queue and is delivered marked steered.
	s := r.cl.conn.session(id)
	s.beginTurn(context.Background(), func() {})
	r.cl.ok("_abhed/session/steer", map[string]any{"sessionId": id, "text": "faster"}, nil)
	r.cl.ok("_abhed/queue/list", map[string]any{"sessionId": id}, &q)
	if len(q.Items) != 1 {
		t.Fatalf("queue while running: %v", q.Items)
	}
	item := q.Items[0].(map[string]any)
	r.cl.ok("_abhed/queue/cancel", map[string]any{"sessionId": id, "id": item["id"]}, nil)
	s.endTurn()
}

// §2.6: the agent cannot change the editor's own files, nor a file with
// unsaved changes in Studio; its commands cannot write the editor's files.
func TestRuleAgentCannotReachTheEditor(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"mode":"accept-edits","allow":["bash(touch *)"]}`)
	vscode := r.write(".vscode/settings.json", "{}")
	hooks := r.write(".git/hooks/README", "hooks")
	dirty := r.write("draft.md", "mine")
	r.model.script(
		callTool("c1", "read", map[string]any{"path": vscode}),
		callTool("c2", "write", map[string]any{"path": vscode, "content": `{"evil":true}`}),
		callTool("c3", "read", map[string]any{"path": dirty}),
		callTool("c4", "write", map[string]any{"path": dirty, "content": "agent's"}),
		callTool("c5", "bash", map[string]any{"command": "touch " + hooks[:len(hooks)-len("README")] + "pre-commit"}),
		say("done"))
	id := r.open()
	r.cl.ok("_abhed/buffers/dirty", map[string]any{"sessionId": id, "paths": []string{dirty}}, nil)
	r.prompt(id, "try it")
	if r.read(".vscode/settings.json") != "{}" {
		t.Fatal("the agent changed .vscode/settings.json")
	}
	if r.read("draft.md") != "mine" {
		t.Fatal("the agent wrote over a file with unsaved changes")
	}
	if _, err := os.Stat(filepath.Join(r.ws, ".git", "hooks", "pre-commit")); err == nil {
		t.Fatal("the agent's command wrote a git hook")
	}
	obs, _ := r.recorded(id, agent.EvObservation)
	var said []string
	for _, o := range obs {
		said = append(said, o["content"].(string))
	}
	joined := strings.Join(said, "\n")
	if !strings.Contains(joined, "editor's own configuration") || !strings.Contains(joined, "unsaved changes") {
		t.Fatalf("observations: %s", joined)
	}
}

// §2.6 holds whatever the case of the name and through links the agent makes,
// and its commands cannot move .git out from under the rules.
func TestRuleEditorFilesByCaseLinkAndRename(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"mode":"accept-edits","allow":["bash(ln *)","bash(mv *)"]}`)
	r.write(".vscode/settings.json", "{}")
	r.write(".git/config", "orig")
	r.write(".git/hooks/README", "hooks")
	at := func(rel string) string { return filepath.Join(r.ws, filepath.FromSlash(rel)) }
	r.model.script(
		callTool("c1", "write", map[string]any{"path": at(".VSCode/launch.json"), "content": "{}"}),
		callTool("c2", "write", map[string]any{"path": at(".GIT/config"), "content": "[core]\n\tfsmonitor = touch /tmp/x\n"}),
		callTool("c3", "bash", map[string]any{"command": "ln -s .vscode cfg"}),
		callTool("c3b", "bash", map[string]any{"command": "ln -s .git g"}),
		callTool("c4", "write", map[string]any{"path": at("cfg/tasks.json"), "content": "{}"}),
		callTool("c5", "write", map[string]any{"path": at("g/hooks/pre-commit"), "content": "#!/bin/sh\n"}),
		callTool("c6", "write", map[string]any{"path": at("team.Code-Workspace"), "content": "{}"}),
		callTool("c7", "bash", map[string]any{"command": "mv .git .git2"}),
		say("done"))
	id := r.open()
	r.prompt(id, "try it")
	for _, f := range []string{".vscode/launch.json", ".VSCode/launch.json", ".vscode/tasks.json", ".git/hooks/pre-commit", "team.Code-Workspace"} {
		if _, err := os.Stat(at(f)); err == nil {
			t.Errorf("the agent wrote %s", f)
		}
	}
	if info, err := os.Lstat(at("cfg")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		obs, _ := r.recorded(id, agent.EvObservation)
		t.Fatalf("the links were not made, so they were not tried: %v %v", err, obs)
	}
	if r.read(".git/config") != "orig" {
		t.Error("the agent changed .git/config")
	}
	if _, err := os.Stat(at(".git2")); err == nil {
		t.Error("the agent's command moved .git")
	}
}

// §2.6 holds in a repository nested in the workspace, since the editor's git
// runs there too; on macOS also in one the agent makes during the session.
func TestRuleNestedRepositoryIsTheEditors(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"mode":"accept-edits","allow":["bash(touch *)","bash(mkdir *)"]}`)
	r.write("sub/.git/config", "orig")
	r.write("sub/.git/hooks/README", "hooks")
	at := func(rel string) string { return filepath.Join(r.ws, filepath.FromSlash(rel)) }
	r.model.script(
		callTool("c1", "write", map[string]any{"path": at("sub/.git/hooks/pre-commit"), "content": "#!/bin/sh\n"}),
		callTool("c2", "write", map[string]any{"path": at("sub/.GIT/config"), "content": "[core]\n"}),
		callTool("c3", "bash", map[string]any{"command": "touch " + at("sub/.git/hooks/post-commit")}),
		callTool("c4", "bash", map[string]any{"command": "mkdir -p " + at("later/.git/hooks")}),
		callTool("c5", "bash", map[string]any{"command": "touch " + at("sub/ok.txt")}),
		say("done"))
	id := r.open()
	r.prompt(id, "try it")
	gone := []string{"sub/.git/hooks/pre-commit", "sub/.git/hooks/post-commit"}
	if runtime.GOOS == "darwin" {
		gone = append(gone, "later/.git")
	}
	for _, f := range gone {
		if _, err := os.Stat(at(f)); err == nil {
			t.Errorf("the agent made %s", f)
		}
	}
	if r.read("sub/.git/config") != "orig" {
		t.Error("the agent changed sub/.git/config")
	}
	if _, err := os.Stat(at("sub/ok.txt")); err != nil {
		t.Fatal("the sandboxed command did not run, so nothing was tried")
	}
}

// §2.6 holds for a workspace opened through a link, as /tmp is on macOS: the
// rules name its real path too.
func TestRuleEditorFilesThroughALinkedWorkspace(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"mode":"accept-edits","allow":["bash(touch *)"]}`)
	r.write(".vscode/settings.json", "{}")
	r.write(".git/hooks/README", "hooks")
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	link := filepath.Join(dir, "ws")
	if err := os.Symlink(r.ws, link); err != nil {
		t.Fatal(err)
	}
	r.model.script(
		callTool("c1", "bash", map[string]any{"command": "touch " + filepath.Join(link, ".git", "hooks", "pre-commit")}),
		callTool("c2", "bash", map[string]any{"command": "touch " + filepath.Join(r.ws, ".git", "hooks", "post-commit")}),
		callTool("c3", "write", map[string]any{"path": filepath.Join(link, ".vscode", "tasks.json"), "content": "{}"}),
		callTool("c4", "bash", map[string]any{"command": "touch " + filepath.Join(link, "notes.txt")}),
		say("done"))
	var res struct {
		SessionID string `json:"sessionId"`
	}
	r.cl.ok("session/new", map[string]any{"cwd": link, "mcpServers": []any{}}, &res)
	r.prompt(res.SessionID, "try it")
	for _, f := range []string{".git/hooks/pre-commit", ".git/hooks/post-commit", ".vscode/tasks.json"} {
		if _, err := os.Stat(filepath.Join(r.ws, filepath.FromSlash(f))); err == nil {
			t.Errorf("the agent wrote %s through the linked workspace", f)
		}
	}
	if _, err := os.Stat(filepath.Join(r.ws, "notes.txt")); err != nil {
		obs, _ := r.recorded(res.SessionID, agent.EvObservation)
		t.Fatalf("the sandboxed command did not run, so nothing was tried: %v", obs)
	}
}

// A password typed ahead into Studio's terminal is echoed by the terminal
// before read -s turns echo off. Typed while another program (sleep) has the
// terminal, it is not a shell line, but its echo is kept out of the record;
// edited as it was typed, it is shown as something other than its text, and
// the record keeps none of the shell's output.
func TestStudioInteractiveTerminalWithholdsTypedAheadPasswords(t *testing.T) {
	for _, tc := range []struct {
		name, wait, keys, done string
		secret                 []string
		whole, program, byKey  bool
	}{
		{"edited", "end=$((SECONDS+2)); while ((SECONDS < end)); do :; done", "swordfiX\x7fsh99\n", "late 11", []string{"swordfi", "sh99"}, true, false, false},
		{"while a program runs", "sleep 2", "hunter55\n", "late 8", []string{"hunter55"}, false, true, false},
		// An editor sends each key in its own input.
		{"key by key", "sleep 2", "Rp1AheadSecretXy\r", "late 16", []string{"Rp1AheadSecretXy"}, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStudioRig(t, "")
			id := r.open()
			var term struct {
				TerminalID string `json:"terminalId"`
				Tier       string `json:"tier"`
			}
			from := r.cl.mark()
			r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "interactive", "cols": 80, "rows": 24}, &term)
			if term.TerminalID == "" || term.Tier == "none" {
				t.Fatalf("create: %+v", term)
			}
			prompted := func(s string) bool {
				p := strings.TrimRight(termline.PlainText([]byte(s)), " ")
				return strings.HasSuffix(p, "$") || strings.HasSuffix(p, "#")
			}
			waitShell := func(from int, pred func(string) bool) string {
				t.Helper()
				var got strings.Builder
				r.cl.waitFor(from, "shell output", func(m rpcMessage) bool {
					got.WriteString(terminalOutput([]rpcMessage{m}, term.TerminalID))
					return pred(got.String())
				})
				return got.String()
			}
			waitShell(from, prompted)
			typeLine := func(keys string, until func(string) bool) string {
				t.Helper()
				at := r.cl.mark()
				r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": keys}, nil)
				return waitShell(at, until)
			}
			typeLine(`echo BU""SY; `+tc.wait+`; read -s pw; echo "late ${#pw}"`+"\r",
				func(s string) bool { return strings.Contains(s, "BUSY") })
			if tc.program {
				// BUSY shows before bash starts sleep; type once sleep has the terminal, so
				// the line is judged as typed into another program, not as the shell's.
				waitForProgram(t, r, term.TerminalID)
			}
			done := func(s string) bool { return strings.Contains(s, tc.done) && prompted(s) }
			var out string
			if tc.byKey {
				at := r.cl.mark()
				for _, k := range tc.keys {
					r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": string(k)}, nil)
				}
				out = waitShell(at, done)
			} else {
				out = typeLine(tc.keys, done)
			}
			if !strings.Contains(out, tc.secret[0]) {
				t.Fatalf("the terminal did not echo the line typed ahead, so nothing was tried: %q", out)
			}
			at := r.cl.mark()
			r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "exit\r"}, nil)
			r.cl.waitFor(at, "the shell's exit", func(m rpcMessage) bool { return m.Method == "_abhed/terminal/exit" })
			time.Sleep(2 * termline.EchoWait)
			for _, ev := range r.events(id) {
				for _, pw := range tc.secret {
					if strings.Contains(string(ev.Payload), pw) {
						t.Fatalf("a password reached the record: %s %s", ev.Type, ev.Payload)
					}
				}
			}
			obs, _ := r.recorded(id, agent.EvObservation)
			c, _ := obs[len(obs)-1]["content"].(string)
			if whole := strings.Contains(c, "output withheld"); whole != tc.whole {
				t.Fatalf("the whole output withheld: %v, want %v: %q", whole, tc.whole, c)
			}
		})
	}
}

// waitForProgram waits until a program other than the shell has the Studio
// terminal, as the engine judges it from the terminal's foreground group.
func waitForProgram(t *testing.T, r *studioRig, terminalID string) {
	t.Helper()
	r.cl.conn.termMu.Lock()
	sh := r.cl.conn.terms[terminalID].shell
	r.cl.conn.termMu.Unlock()
	if !sh.local {
		return
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if fg, _, ok := ttyNow(sh.tty); ok && sh.isProgram(fg) {
			return
		}
	}
	t.Fatal("no program took the terminal from the shell")
}

// §7.1 interactive: the shell runs whole under the sandbox; each line is put
// to the deny rules at its Enter and recorded as the person's, and a line the
// terminal did not show, as at a password prompt, is recorded withheld.
func TestStudioInteractiveTerminal(t *testing.T) {
	r := newStudioRig(t, `,"permissions":{"deny":["bash(curl *)"]}`)
	id := r.open()
	var term struct {
		TerminalID string `json:"terminalId"`
		Tier       string `json:"tier"`
	}
	from := r.cl.mark()
	r.cl.ok("_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "interactive", "cols": 80, "rows": 24}, &term)
	if term.TerminalID == "" || term.Tier == "none" {
		t.Fatalf("create: %+v", term)
	}
	prompted := func(s string) bool {
		p := strings.TrimRight(termline.PlainText([]byte(s)), " ")
		return strings.HasSuffix(p, "$") || strings.HasSuffix(p, "#")
	}
	waitShell := func(from int, pred func(string) bool) {
		t.Helper()
		var got strings.Builder
		r.cl.waitFor(from, "shell output", func(m rpcMessage) bool {
			got.WriteString(terminalOutput([]rpcMessage{m}, term.TerminalID))
			return pred(got.String())
		})
	}
	waitShell(from, prompted)
	typeLine := func(keys string, until func(string) bool) {
		t.Helper()
		at := r.cl.mark()
		r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": keys}, nil)
		waitShell(at, until)
	}
	typeLine("echo hi-$((40+2))\r", func(s string) bool { return strings.Contains(s, "hi-42") && prompted(s) })
	// Each command is typed once the prompt is back, as a person would: a line
	// sent sooner is typed ahead, and withheld however harmless.
	typeLine("curl example.com\r", func(s string) bool { return strings.Contains(s, "Denied") && prompted(s) })
	// Echo is off before READY shows, so the password is typed into it.
	typeLine(`stty -echo; echo RE""ADY; read pw; stty echo; echo "got ${#pw}"`+"\r", func(s string) bool { return strings.Contains(s, "READY") })
	typeLine("hunter22\r", func(s string) bool { return strings.Contains(s, "got 8") && prompted(s) })
	// Typed ahead: the password is sent before read -s has turned echo off.
	// It ends in Ctrl-J: a bare CR that lands while bash's line editor still
	// has the terminal is not turned into a newline, and read waits for one.
	at := r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": `read -s pw; echo "also ${#pw}"` + "\r"}, nil)
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "hunter33\n"}, nil)
	waitShell(at, func(s string) bool { return strings.Contains(s, "also 8") && prompted(s) })
	// Typed while the line before read -s still runs, with echo on: the
	// terminal shows it, and the record's copy of the output must not.
	typeLine(`echo BU""SY; end=$((SECONDS+2)); while ((SECONDS < end)); do :; done; read -s pw; echo "late ${#pw}"`+"\r",
		func(s string) bool { return strings.Contains(s, "BUSY") })
	typeLine("hunter44\n", func(s string) bool {
		if strings.Contains(s, "late 8") && !strings.Contains(s, "hunter44") {
			t.Fatal("the terminal did not echo the line typed ahead, so nothing was tried")
		}
		return strings.Contains(s, "late 8") && prompted(s)
	})
	at = r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "exit\r"}, nil)
	r.cl.waitFor(at, "the shell's exit", func(m rpcMessage) bool { return m.Method == "_abhed/terminal/exit" })
	time.Sleep(2 * termline.EchoWait)

	inputs, actors := r.recorded(id, agent.EvTerminalInput)
	var lines []string
	withheld := false
	for i, in := range inputs {
		if actors[i] != agent.ActorUser {
			t.Fatalf("terminal.input by %s", actors[i])
		}
		if l, ok := in["line"].(string); ok {
			lines = append(lines, l)
		}
		withheld = withheld || in["withheld"] != nil
	}
	if !slices.Contains(lines, "echo hi-$((40+2))") || !withheld {
		t.Fatalf("terminal.input: %v", inputs)
	}
	for _, ev := range r.events(id) {
		if strings.Contains(string(ev.Payload), "hunter22") || strings.Contains(string(ev.Payload), "hunter33") || strings.Contains(string(ev.Payload), "hunter44") {
			t.Fatalf("the unechoed line reached the record: %s %s", ev.Type, ev.Payload)
		}
	}
	denied, _ := r.recorded(id, agent.EvActionDenied)
	if len(denied) != 1 || denied[0]["by"] != "policy" {
		t.Fatalf("action.denied: %v", denied)
	}
	obs, _ := r.recorded(id, agent.EvObservation)
	if !strings.Contains(obs[len(obs)-1]["content"].(string), "interactive Abhed terminal") {
		t.Fatalf("the shell's end: %v", obs[len(obs)-1])
	}
}

// Only a managed file that sets deny rules forces line mode on Studio's
// terminal; one that only turns web search on leaves it interactive.
func TestStudioInteractiveTerminalUnderManagedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, managedBody string
		refused           bool
	}{
		{"managed deny", `{"permissions":{"deny":["bash(*curl*)"]}}`, true},
		{"managed web search", `{"web_search":{"enabled":true}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStudioRig(t, "")
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.managedBody), 0o644); err != nil {
				t.Fatal(err)
			}
			old := managed.ConfigFile
			managed.ConfigFile = path
			t.Cleanup(func() { managed.ConfigFile = old })
			id := r.open()
			params := map[string]any{"sessionId": id, "mode": "interactive", "cols": 80, "rows": 24}
			if tc.refused {
				if msg := r.cl.refused(errPolicy, "_abhed/terminal/create", params); !strings.Contains(msg, "lines mode") {
					t.Fatalf("refusal does not name lines mode: %q", msg)
				}
				return
			}
			var term struct {
				TerminalID string `json:"terminalId"`
			}
			r.cl.ok("_abhed/terminal/create", params, &term)
			if term.TerminalID == "" {
				t.Fatal("no interactive terminal opened")
			}
			r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "exit\r"}, nil)
		})
	}
}
