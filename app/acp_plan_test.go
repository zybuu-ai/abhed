package app

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Over ACP: plan mode offers exit_plan; the plan and its runbook are put to
// the editor as a permission request; accepting changes the mode via
// plan-exit and binds the runbook, so the listed step runs without another
// request and a change outside it is asked about, saying so.
func TestStudioRunbookPlan(t *testing.T) {
	r := newStudioRig(t, "")
	notes, other := filepath.Join(r.ws, "notes.txt"), filepath.Join(r.ws, "other.txt")
	r.model.script(
		callTool("c1", "exit_plan", map[string]any{"plan": "1. write notes", "runbook": []any{
			map[string]any{"tool": "write", "target": notes, "args": map[string]any{"content": "x"}},
		}}),
		callTool("c2", "write", map[string]any{"path": notes, "content": "x"}),
		callTool("c3", "write", map[string]any{"path": other, "content": "x"}),
		say("done"))
	var asks []string
	r.cl.answering(func(method string, params json.RawMessage) any {
		if method != "session/request_permission" {
			return map[string]any{}
		}
		asks = append(asks, string(params))
		if strings.Contains(string(params), "Proceed with this plan?") {
			return chosen(params, "allow_once")
		}
		return chosen(params, "reject_once")
	})
	id := r.open()
	r.cl.ok("session/set_mode", map[string]any{"sessionId": id, "modeId": "plan"}, nil)
	r.prompt(id, "plan the notes")
	if len(asks) != 2 || !strings.Contains(asks[0], "Runbook") || !strings.Contains(asks[0], `"planHash"`) ||
		!strings.Contains(asks[1], "not in the approved plan") {
		t.Fatalf("asks %v", asks)
	}
	if r.read("notes.txt") != "x" || r.read("other.txt") != "" {
		t.Fatalf("notes %q other %q", r.read("notes.txt"), r.read("other.txt"))
	}
	decided, _ := r.recorded(id, agent.EvPlanDecided)
	changed, _ := r.recorded(id, agent.EvModeChanged)
	steps, _ := r.recorded(id, agent.EvPlanStep)
	if len(decided) != 1 || decided[0]["decision"] != "accept" || decided[0]["to_mode"] != "default" || decided[0]["plan_hash"] == nil {
		t.Fatalf("plan.decided %v", decided)
	}
	if len(changed) != 2 || changed[1]["via"] != agent.ViaPlanExit || changed[1]["to"] != "default" {
		t.Fatalf("mode.changed %v", changed)
	}
	if len(steps) != 1 || steps[0]["by"] != agent.ByPlan || steps[0]["plan_hash"] != decided[0]["plan_hash"] {
		t.Fatalf("plan.step %v", steps)
	}
}

// An accepted plan whose answer the record does not take changes nothing
// over ACP: the session stays in plan mode and the plan still waits.
func TestStudioPlanAnswerNotRecorded(t *testing.T) {
	r := newStudioRig(t, "")
	notes := filepath.Join(r.ws, "notes.txt")
	r.model.script(
		callTool("c1", "exit_plan", map[string]any{"plan": "1. write notes", "runbook": []any{
			map[string]any{"tool": "write", "target": notes, "args": map[string]any{"content": "x"}},
		}}),
		say("done"))
	var id string
	r.cl.answering(func(method string, params json.RawMessage) any {
		if method != "session/request_permission" {
			return map[string]any{}
		}
		r.cl.conn.session(id).parts.Loop.Recorder.Gate = func() error { return errors.New("the record is gone") }
		return chosen(params, "allow_once")
	})
	id = r.open()
	r.cl.ok("session/set_mode", map[string]any{"sessionId": id, "modeId": "plan"}, nil)
	r.prompt(id, "plan the notes")
	loop := r.cl.conn.session(id).parts.Loop
	if loop.Policy.Mode != "plan" {
		t.Fatalf("mode %s after an answer the record did not take", loop.Policy.Mode)
	}
	if _, _, bound := loop.BoundRunbook(); bound {
		t.Fatal("a runbook was bound")
	}
	if _, ok := loop.PendingPlan(); !ok {
		t.Fatal("the plan no longer waits")
	}
}
