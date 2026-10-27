package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// askCounter approves everything and counts how often it was asked.
type askCounter struct{ asked int }

func (a *askCounter) Approve(context.Context, string, json.RawMessage, policy.Result) (bool, error) {
	a.asked++
	return true, nil
}

// A path out of the workspace can never succeed, so nobody is asked to
// approve it, and the model still gets the correction.
func TestDoomedCallIsNotPutToAPerson(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("write", map[string]string{"path": "../new.txt", "content": "x"})}},
		{text: "ok"},
	}, policy.ModeDefault, true)
	counter := &askCounter{}
	l.Approver = counter

	l.Run(context.Background(), "write a file")

	if counter.asked != 0 {
		t.Fatalf("asked %d times about a call that could not succeed", counter.asked)
	}
	evs, _ := store.Events("sess1")
	for _, e := range evs {
		if e.Type != EvActionRequested {
			continue
		}
		var p ActionRequested
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.RequiresApproval {
			t.Fatal("the record says approval was required for a call that was never put to anyone")
		}
	}
	adapter := l.Adapter.(*scriptedAdapter)
	last := adapter.gotRequests[len(adapter.gotRequests)-1]
	found := false
	for _, m := range last.Messages {
		if m.Role == model.RoleTool && strings.Contains(m.Content, "outside this session's workspace") {
			found = true
		}
	}
	if !found {
		t.Fatal("the model was not told how to correct the path")
	}
}

// A deny rule still reads as a denial, not as a failed precheck.
func TestPrecheckDoesNotMaskADeny(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("write", map[string]string{"path": "new.txt", "content": "x"})}},
		{text: "ok"},
	}, policy.ModePlan, true)

	l.Run(context.Background(), "write a file")

	evs, _ := store.Events("sess1")
	if !hasEvent(evs, EvActionDenied) {
		t.Fatal("plan mode must still record the write as denied")
	}
}

// In accept-edits a write is allowed without asking; one the tool would refuse,
// into Abhed's own state, is refused before it is recorded as approved.
func TestAllowedDoomedWriteIsNotRecordedApproved(t *testing.T) {
	dir := tempDir(t)
	if err := os.MkdirAll(filepath.Join(dir, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	l, store := harnessIn(t, dir, []scriptedTurn{
		{calls: []model.ToolCall{call("write", map[string]string{"path": filepath.Join(dir, ".abhed", "config.json"), "content": "{}"})}},
		{text: "ok"},
	}, policy.ModeAcceptEdits, true)
	l.Run(context.Background(), "write the config")
	evs, _ := store.Events("sess1")
	for _, e := range evs {
		if e.Type == EvActionApproved {
			t.Fatalf("recorded approved: %s", e.Payload)
		}
	}
	denied := payloads[map[string]string](evs, EvActionDenied)
	if len(denied) != 1 || denied[0]["step"] != "precheck" || !strings.Contains(denied[0]["reason"], "Abhed's own state") {
		t.Fatalf("denied: %+v", denied)
	}
}

// An approved edit that failed without effect hands its approval to a retry
// of the very same change, once, recorded as carried; a change to another
// place, other new text or other indentation is asked again.
func TestFailedEditDoesNotSpendItsApproval(t *testing.T) {
	dir := tempDir(t)
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package a\n\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := func(old, new string) model.ToolCall {
		return call("edit", map[string]string{"path": p, "old_string": old, "new_string": new})
	}
	turns := []scriptedTurn{
		{calls: []model.ToolCall{call("read", map[string]string{"path": p})}},
		{calls: []model.ToolCall{edit("var x = 1\n", "var x = 2\n")}}, // asked; the file changes meanwhile, so it fails
		{calls: []model.ToolCall{call("read", map[string]string{"path": p})}},
		{calls: []model.ToolCall{edit("var x = 1  \n", "var x = 2\n")}}, // the same change, spaces at a line's end aside: carried
		{text: "done"},
	}
	l, store := harnessIn(t, dir, turns, policy.ModeDefault, true)
	counter := &askCounter{}
	// The file changes on disk while the person is asked, so the approved edit fails with no effect.
	l.Approver = approverFunc(func(context.Context, policy.Result) (bool, error) {
		counter.asked++
		if counter.asked == 1 {
			_ = os.WriteFile(p, []byte("package a\n\n// touched\nvar x = 1\n"), 0o644)
		}
		return true, nil
	})
	if _, err := l.Run(context.Background(), "change x"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); counter.asked != 1 || string(got) != "package a\n\n// touched\nvar x = 2\n" {
		t.Fatalf("asked %d; file %q", counter.asked, got)
	}
	evs, _ := store.Events("sess1")
	carried := 0
	for _, e := range evs {
		if e.Type == EvActionApproved {
			var m map[string]string
			_ = json.Unmarshal(e.Payload, &m)
			if m["carried_from"] != "" {
				carried++
				if m["by"] != ByCarried || e.Actor != ActorSystem {
					t.Fatalf("carried approval by %q, actor %s", m["by"], e.Actor)
				}
			}
		}
	}
	if carried != 1 {
		t.Fatalf("%d carried approvals", carried)
	}
}

// The reviewer's probe: an approved deletion of a line that does not exist
// does not carry to deleting other lines, and a carry never chains.
func TestCarriedApprovalNeverMovesOrChains(t *testing.T) {
	dir := tempDir(t)
	p := filepath.Join(dir, "a.go")
	body := "package a\n\nfunc h() {\n\tcheckAuth()\n\tserve()\n}\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := func(old, new string) model.ToolCall {
		return call("edit", map[string]string{"path": p, "old_string": old, "new_string": new})
	}
	turns := []scriptedTurn{
		{calls: []model.ToolCall{call("read", map[string]string{"path": p})}},
		{calls: []model.ToolCall{edit("\tdebugLog()\n", "")}},
		{calls: []model.ToolCall{edit("\ttraceLog()\n", "")}},
		{calls: []model.ToolCall{edit("\tcheckAuth()\n", "")}},
		{calls: []model.ToolCall{edit("\tcheckAuth()\n", "")}},
		{text: "done"},
	}
	l, store := harnessIn(t, dir, turns, policy.ModeDefault, true)
	counter := &rejectAfter{allow: 1}
	l.Approver = counter
	if _, err := l.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	// Only the first was approved; the others were asked and refused.
	if got, _ := os.ReadFile(p); string(got) != body || counter.asked != 4 {
		t.Fatalf("asked %d; file %q", counter.asked, got)
	}
	evs, _ := store.Events("sess1")
	for _, e := range evs {
		var m map[string]string
		if e.Type == EvActionApproved && json.Unmarshal(e.Payload, &m) == nil && m["carried_from"] != "" {
			t.Fatalf("an approval carried: %s", e.Payload)
		}
	}
}

// rejectAfter approves the first allow asks and rejects the rest, counting them.
type rejectAfter struct{ allow, asked int }

func (a *rejectAfter) Approve(context.Context, string, json.RawMessage, policy.Result) (bool, error) {
	a.asked++
	return a.asked <= a.allow, nil
}

// The reviewer's probe: an approved deletion that failed for matching twice
// does not carry to the same deletion with replace_all.
func TestCarriedApprovalNeedsTheSameReplaceAll(t *testing.T) {
	dir := tempDir(t)
	p := filepath.Join(dir, "a.go")
	body := "package a\n\nfunc h() {\n\tcheckAuth()\n\tcheckAuth()\n}\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	turns := []scriptedTurn{
		{calls: []model.ToolCall{call("read", map[string]string{"path": p})}},
		{calls: []model.ToolCall{call("edit", map[string]any{"path": p, "old_string": "\tcheckAuth()\n", "new_string": ""})}},
		{calls: []model.ToolCall{call("edit", map[string]any{"path": p, "old_string": "\tcheckAuth()\n", "new_string": "", "replace_all": true})}},
		{text: "done"},
	}
	l, _ := harnessIn(t, dir, turns, policy.ModeDefault, true)
	counter := &rejectAfter{allow: 1}
	l.Approver = counter
	if _, err := l.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); counter.asked != 2 || string(got) != body {
		t.Fatalf("asked %d; file %q", counter.asked, got)
	}
}
