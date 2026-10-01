package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
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
	r.cl.refused(errRefused, "_abhed/terminal/create", map[string]any{"sessionId": id, "mode": "interactive"})
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
