package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func (wb *workbench) typeLine(req ptyStartRequest) ptyStartResponse {
	wb.t.Helper()
	req.Cols, req.Rows = 80, 24
	rec := wb.send("acme", "POST", "pty", req)
	if rec.Code != http.StatusOK {
		wb.t.Fatalf("pty start: %d %s", rec.Code, rec.Body)
	}
	var out ptyStartResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// decisions returns the approvals and denials recorded for call id.
func (wb *workbench) decisions(id string) []agent.Event {
	var out []agent.Event
	for _, e := range wb.events() {
		if (e.Type == agent.EvActionApproved || e.Type == agent.EvActionDenied) && strings.Contains(string(e.Payload), `"call_id":"`+id+`"`) {
			out = append(out, e)
		}
	}
	return out
}

func requestedIDs(wb *workbench, command string) []string {
	var ids []string
	for _, e := range wb.events() {
		if e.Type != agent.EvActionRequested {
			continue
		}
		var a agent.ActionRequested
		_ = json.Unmarshal(e.Payload, &a)
		var args struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(a.Args, &args)
		if args.Command == command {
			ids = append(ids, a.CallID)
		}
	}
	return ids
}

// A destructive line typed into the line-by-line terminal is not run on
// Enter: it waits for an explicit confirmation, and a decline is recorded.
func TestTerminalLineConfirmsDestructiveCommands(t *testing.T) {
	wb := manualBench(t, nil)
	wb.write("keep.py", "x")
	const command = "rm -rf keep.py"
	kept := func() bool { _, err := os.Stat(filepath.Join(wb.workspace, "keep.py")); return err == nil }

	// A client that sends no answer is told to confirm, and nothing runs or is recorded.
	first := wb.typeLine(ptyStartRequest{Command: command})
	if first.Confirm == "" || first.ID != "" || first.Denied != "" {
		t.Fatalf("unanswered destructive line: %+v", first)
	}
	if !strings.Contains(first.Confirm, "always requires confirmation") {
		t.Fatalf("confirm reason: %q", first.Confirm)
	}
	if !kept() || len(requestedIDs(wb, command)) != 0 {
		t.Fatal("an unconfirmed destructive line ran or was recorded")
	}

	// Declined: not run, recorded as the person's refusal.
	declined := wb.typeLine(ptyStartRequest{Command: command, Declined: true})
	if declined.Denied == "" || declined.Confirm != "" || !kept() {
		t.Fatalf("declined: %+v, file kept %v", declined, kept())
	}
	ds := wb.decisions(declined.ID)
	if len(ds) != 1 || ds[0].Type != agent.EvActionDenied || ds[0].Actor != agent.ActorUser ||
		!strings.Contains(string(ds[0].Payload), `"step":"destructive"`) {
		t.Fatalf("declined record: %+v", ds)
	}

	// Confirmed: runs, and the record says it was confirmed.
	confirmed := wb.typeLine(ptyStartRequest{Command: command, Confirmed: true})
	if confirmed.ID == "" || confirmed.Denied != "" || confirmed.Confirm != "" {
		t.Fatalf("confirmed: %+v", confirmed)
	}
	if _, exit := wb.ptyOutput(confirmed.ID, ""); exit != "0" || kept() {
		t.Fatalf("confirmed rm: exit %q, file kept %v", exit, kept())
	}
	ds = wb.decisions(confirmed.ID)
	if len(ds) != 1 || ds[0].Type != agent.EvActionApproved {
		t.Fatalf("confirmed record: %+v", ds)
	}
	var p map[string]string
	_ = json.Unmarshal(ds[0].Payload, &p)
	if p["by"] != "user" || p["step"] != "destructive" || p["confirmed"] != "true" {
		t.Fatalf("approval payload: %v", p)
	}
	if got := requestedIDs(wb, command); len(got) != 2 {
		t.Fatalf("requests recorded: %v, want the declined and the confirmed one", got)
	}
}

// Other asks are still answered by typing the line; a deny holds even when
// the line comes back confirmed; both answers at once is a bad request.
func TestTerminalLineConfirmationLeavesOtherDecisions(t *testing.T) {
	wb := manualBench(t, nil)
	plain := wb.typeLine(ptyStartRequest{Command: "touch made.txt"})
	if plain.ID == "" || plain.Confirm != "" || plain.Denied != "" {
		t.Fatalf("plain line: %+v", plain)
	}
	wb.ptyOutput(plain.ID, "")
	if _, err := os.Stat(filepath.Join(wb.workspace, "made.txt")); err != nil {
		t.Fatal("a line that needs no confirmation did not run")
	}
	var p map[string]string
	ds := wb.decisions(plain.ID)
	if len(ds) != 1 {
		t.Fatalf("plain record: %+v", ds)
	}
	_ = json.Unmarshal(ds[0].Payload, &p)
	if _, found := p["confirmed"]; found || p["by"] != "user" {
		t.Fatalf("plain approval payload: %v", p)
	}

	// A decline for a line that needed no confirmation: not run, not recorded.
	if declined := wb.typeLine(ptyStartRequest{Command: "touch declined.txt", Declined: true}); declined.Denied == "" || declined.ID != "" {
		t.Fatalf("declined ordinary line: %+v", declined)
	}
	if _, err := os.Stat(filepath.Join(wb.workspace, "declined.txt")); err == nil || len(requestedIDs(wb, "touch declined.txt")) != 0 {
		t.Fatal("a declined ordinary line ran or was recorded")
	}

	if denied := wb.typeLine(ptyStartRequest{Command: "shutdown -h now", Confirmed: true}); !strings.Contains(denied.Denied, "Denied") || denied.Confirm != "" {
		t.Fatalf("a denied line confirmed: %+v", denied)
	}
	if rec := wb.send("acme", "POST", "pty", ptyStartRequest{Command: "rm -rf x", Confirmed: true, Declined: true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("confirmed and declined: %d", rec.Code)
	}
}

// A cd line that Tab completed, name quoted, moves the terminal there; one the
// server cannot follow says so instead of leaving the next line elsewhere.
func TestTerminalLineFollowsAQuotedCd(t *testing.T) {
	wb := manualBench(t, nil)
	if err := os.MkdirAll(filepath.Join(wb.workspace, "packages", "web app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := wb.typeLine(ptyStartRequest{Command: `cd packages/web\ app/`}); got.Cwd != "packages/web app" || got.Note != "" {
		t.Fatalf("quoted cd: %+v", got)
	}
	got := wb.typeLine(ptyStartRequest{Command: "cd missing"})
	if got.Cwd != "packages/web app" || !strings.Contains(got.Note, "no such directory: missing") || !strings.Contains(got.Note, "still in packages/web app") {
		t.Fatalf("cd to a missing folder: %+v", got)
	}
	if got := wb.typeLine(ptyStartRequest{Command: "cd"}); got.Cwd != "." || got.Note != "" {
		t.Fatalf("bare cd: %+v", got)
	}
	// $'...' is read without a shell, and says so when the folder is missing.
	if got := wb.typeLine(ptyStartRequest{Command: `cd $'missing'`}); got.ID == "" || !strings.Contains(got.Note, "no such directory: $'missing'") {
		t.Fatalf("ANSI-C quoted cd: %+v", got)
	}
	if got := wb.typeLine(ptyStartRequest{Command: `cd	$'packages/web app'`}); got.Cwd != "packages/web app" {
		t.Fatalf("cd, a tab and an ANSI-C quote: %+v", got)
	}
	wb.typeLine(ptyStartRequest{Command: "cd"})
	// A cd that runs in a shell, and that the server cannot follow, says so as it ends.
	ran := wb.typeLine(ptyStartRequest{Command: `cd "$HOME"`})
	out, exit := wb.ptyOutput(ran.ID, "")
	if exit != "0" || !strings.Contains(out, "cd: not followed: only a line that is just `cd <folder>` is") || !strings.Contains(out, "still in .") {
		t.Fatalf("unfollowed cd in a shell: exit %s, output %q", exit, out)
	}
	recorded := false
	for _, e := range wb.events() {
		recorded = recorded || e.Type == agent.EvObservation && strings.Contains(string(e.Payload), ran.ID) && strings.Contains(string(e.Payload), "not followed")
	}
	if !recorded {
		t.Fatal("the note is not in the command's record")
	}
	// A cd behind a command that failed never ran, so nothing moves or is said.
	failed := wb.typeLine(ptyStartRequest{Command: "false && cd packages"})
	if out, exit := wb.ptyOutput(failed.ID, ""); exit != "1" || strings.Contains(out, "cd:") {
		t.Fatalf("failed command: exit %s, output %q", exit, out)
	}
	if got := wb.typeLine(ptyStartRequest{Command: "cd nowhere"}); !strings.Contains(got.Note, "still in .") {
		t.Fatalf("the failed command moved the terminal: %+v", got)
	}
}

// A note quotes the typed line; its control characters reach neither the
// terminal nor the record.
func TestTerminalNoteNeutralisesControlCharacters(t *testing.T) {
	ch := make(chan []byte, 1)
	run := &ptyRun{subs: map[chan []byte]struct{}{ch: {}}}
	run.note("cd: \"a\x1b]0;x\x07\u009bb\" is not followed")
	sent := string(<-ch)
	body := strings.TrimSuffix(strings.TrimPrefix(sent, "\r\n\x1b[33m"), "\x1b[0m")
	if strings.ContainsAny(body, "\x1b\x07\u009b") || body != `cd: "a?]0;x??b" is not followed` || string(run.record) != sent {
		t.Fatalf("note sent %q, recorded %q", sent, run.record)
	}
}
