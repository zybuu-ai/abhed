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

	if denied := wb.typeLine(ptyStartRequest{Command: "shutdown -h now", Confirmed: true}); !strings.Contains(denied.Denied, "Denied") || denied.Confirm != "" {
		t.Fatalf("a denied line confirmed: %+v", denied)
	}
	if rec := wb.send("acme", "POST", "pty", ptyStartRequest{Command: "rm -rf x", Confirmed: true, Declined: true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("confirmed and declined: %d", rec.Code)
	}
}
