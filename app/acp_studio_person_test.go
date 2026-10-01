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

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/termline"
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
	typeLine("curl example.com\r", func(s string) bool { return strings.Contains(s, "Denied") })
	// Echo is off before READY shows, so the password is typed into it.
	typeLine(`stty -echo; echo RE""ADY; read pw; stty echo; echo "got ${#pw}"`+"\r", func(s string) bool { return strings.Contains(s, "READY") })
	typeLine("hunter22\r", func(s string) bool { return strings.Contains(s, "got 8") })
	// Typed ahead: the password is sent before read -s has turned echo off.
	at := r.cl.mark()
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": `read -s pw; echo "also ${#pw}"` + "\r"}, nil)
	r.cl.ok("_abhed/terminal/input", map[string]any{"terminalId": term.TerminalID, "data": "hunter33\r"}, nil)
	waitShell(at, func(s string) bool { return strings.Contains(s, "also 8") && prompted(s) })
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
		if strings.Contains(string(ev.Payload), "hunter22") || strings.Contains(string(ev.Payload), "hunter33") {
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
