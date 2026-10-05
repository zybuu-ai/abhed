package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func manualLoop(t *testing.T) (*Loop, *MemStore) {
	t.Helper()
	store := NewMemStore()
	pol := policy.New(policy.ModeDefault)
	if err := pol.AddDeny("bash(curl*)"); err != nil {
		t.Fatal(err)
	}
	return &Loop{Tools: tools.NewRegistry(tools.Bash{}), Policy: pol, Recorder: NewRecorder(store, "s-manual", "")}, store
}

func bashArgs(command string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": command})
	return b
}

func decisionsOf(t *testing.T, store *MemStore, id string) []Event {
	t.Helper()
	evs, err := store.Events("s-manual")
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range evs {
		var p map[string]string
		_ = json.Unmarshal(e.Payload, &p)
		if (e.Type == EvActionApproved || e.Type == EvActionDenied) && p["call_id"] == id {
			out = append(out, e)
		}
	}
	return out
}

// A typed destructive command needs an answer; the answer is what the record holds.
func TestManualAuthorizeTypedConfirmsDestructive(t *testing.T) {
	l, store := manualLoop(t)

	tool, refused, confirm, err := l.ManualAuthorizeTyped("u1", bashArgs("rm -rf build"), Unanswered)
	if err != nil || tool != nil || refused != nil || confirm == "" {
		t.Fatalf("unanswered: tool %v refused %v confirm %q err %v", tool, refused, confirm, err)
	}
	if evs, _ := store.Events("s-manual"); len(evs) != 0 {
		t.Fatalf("an unanswered prompt was recorded: %d events", len(evs))
	}

	tool, refused, _, err = l.ManualAuthorizeTyped("u2", bashArgs("rm -rf build"), Declined)
	if err != nil || tool != nil || refused == nil || !refused.IsError {
		t.Fatalf("declined: tool %v refused %v err %v", tool, refused, err)
	}
	if ds := decisionsOf(t, store, "u2"); len(ds) != 1 || ds[0].Type != EvActionDenied || ds[0].Actor != ActorUser {
		t.Fatalf("declined record: %+v", ds)
	}

	tool, refused, _, err = l.ManualAuthorizeTyped("u3", bashArgs("rm -rf build"), Confirmed)
	if err != nil || tool == nil || refused != nil {
		t.Fatalf("confirmed: tool %v refused %v err %v", tool, refused, err)
	}
	ds := decisionsOf(t, store, "u3")
	var p map[string]string
	if len(ds) == 1 {
		_ = json.Unmarshal(ds[0].Payload, &p)
	}
	if len(ds) != 1 || ds[0].Type != EvActionApproved || ds[0].Actor != ActorUser || p["confirmed"] != "true" || p["step"] != "destructive" || p["by"] != "user" {
		t.Fatalf("confirmed record: %+v %v", ds, p)
	}

	// A deny holds whatever the answer; an ordinary ask needs none.
	if _, refused, _, _ := l.ManualAuthorizeTyped("u4", bashArgs("curl http://x"), Confirmed); refused == nil {
		t.Fatal("a confirmed line passed a deny rule")
	}
	if tool, _, confirm, _ := l.ManualAuthorizeTyped("u5", bashArgs("touch a"), Unanswered); tool == nil || confirm != "" {
		t.Fatalf("an ordinary ask was held for confirmation: %q", confirm)
	}
	// Declining a line that needed no confirmation runs and records nothing.
	if tool, refused, _, err := l.ManualAuthorizeTyped("u6", bashArgs("touch a"), Declined); !errors.Is(err, ErrNothingToDecline) || tool != nil || refused != nil {
		t.Fatalf("declined ordinary line: tool %v refused %v err %v", tool, refused, err)
	}
	evs, _ := store.Events("s-manual")
	for _, e := range evs {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if p["call_id"] == "u6" {
			t.Fatalf("a declined ordinary line was recorded: %s", e.Type)
		}
	}
}

// The other manual paths are unchanged: an explorer delete is answered by the
// person's click, and the interactive terminal screens only for a deny.
func TestManualAuthorizeAndScreenAreUnchanged(t *testing.T) {
	l, store := manualLoop(t)
	if tool, refused, err := l.ManualAuthorize("bash", "u1", bashArgs("rm -rf build")); err != nil || tool == nil || refused != nil {
		t.Fatalf("ManualAuthorize: tool %v refused %v err %v", tool, refused, err)
	}
	var p map[string]string
	ds := decisionsOf(t, store, "u1")
	if len(ds) == 1 {
		_ = json.Unmarshal(ds[0].Payload, &p)
	}
	if len(ds) != 1 || ds[0].Type != EvActionApproved || p["by"] != "user" {
		t.Fatalf("ManualAuthorize record: %+v", ds)
	}
	if _, found := p["confirmed"]; found {
		t.Fatalf("ManualAuthorize recorded a confirmation: %v", p)
	}
	if refused, err := l.ManualScreen("u2", "rm -rf build"); err != nil || refused != nil {
		t.Fatalf("ManualScreen refused a destructive line: %v %v", refused, err)
	}
	if refused, err := l.ManualScreen("u3", "ls; curl http://x"); err != nil || refused == nil {
		t.Fatalf("ManualScreen passed a denied command in a chain: %v %v", refused, err)
	}
}

// A request records only the scope a person may choose: none where a step,
// here a hook, forces an ask that must be answered every time.
func TestManualRequestRecordsTheOfferedScope(t *testing.T) {
	l, store := manualLoop(t)
	l.Policy.Hooks = append(l.Policy.Hooks, func(string, json.RawMessage) *policy.Result {
		return &policy.Result{Decision: policy.Ask, Reason: "hook", Scope: "bash(ls *)", Step: "hook"}
	})
	if _, _, err := l.ManualAuthorize("bash", "u1", bashArgs("ls")); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("s-manual")
	for _, e := range evs {
		var a ActionRequested
		if e.Type == EvActionRequested && json.Unmarshal(e.Payload, &a) == nil && a.Scope != "" {
			t.Fatalf("the request offers %q", a.Scope)
		}
	}
}

// weight ranks what ManualAs keeps: a deny, then an ask a rule or hook made,
// then an ask the mode or default made, then an allow.
func TestWeightRanksManualDecisions(t *testing.T) {
	for _, c := range []struct {
		d    policy.Result
		want int
	}{
		{policy.Result{Decision: policy.Deny, Step: "deny"}, 3},
		{policy.Result{Decision: policy.Ask, Step: "ask"}, 2},
		{policy.Result{Decision: policy.Ask, Step: "hook"}, 2},
		{policy.Result{Decision: policy.Ask, Step: "mode"}, 1},
		{policy.Result{Decision: policy.Ask, Step: "default"}, 1},
		{policy.Result{Decision: policy.Allow, Step: "allow"}, 0},
	} {
		if got := weight(c.d); got != c.want {
			t.Errorf("%s at %s: %d, want %d", c.d.Decision, c.d.Step, got, c.want)
		}
	}
}

// A line that opens a construct the shell finishes with later lines, or
// closes one, is left to the shell at the interactive terminal; a denied
// command on a line of its own is still refused.
func TestManualScreenLeavesAnOpenConstructToTheShell(t *testing.T) {
	l, _ := manualLoop(t)
	for i, line := range []string{
		"for f in *; do", "if true; then", "while true; do", "cat <<EOF", "f() {", "case x in",
		"git commit -m \"first line", "echo hi |", "ls &&", "(cd x", "done", "fi", "esac", "}",
	} {
		if refused, err := l.ManualScreen(fmt.Sprintf("o%d", i), line); err != nil || refused != nil {
			t.Errorf("ManualScreen(%q) refused it: %v %v", line, refused, err)
		}
	}
	for i, line := range []string{"curl http://x", "x=c_u_r_l; ${x//_/} http://x"} {
		if refused, err := l.ManualScreen(fmt.Sprintf("d%d", i), line); err != nil || refused == nil {
			t.Errorf("ManualScreen(%q) passed it: %v", line, err)
		}
	}
}

// callMutator mutates only when its arguments say so.
type callMutator struct{ tools.Bash }

func (callMutator) Name() string                         { return "mover" }
func (callMutator) Mutates() bool                        { return false }
func (callMutator) MutatesCall(raw json.RawMessage) bool { return string(raw) == `{"command":"move"}` }

// A person's call is judged by what this call does, as the agent's is: one
// that mutates by its arguments is refused in plan mode.
func TestManualAuthorizeJudgesTheCall(t *testing.T) {
	store := NewMemStore()
	l := &Loop{Tools: tools.NewRegistry(callMutator{}), Policy: policy.New(policy.ModePlan), Recorder: NewRecorder(store, "s-manual", "")}
	if _, refused, err := l.ManualAuthorize("mover", "m1", json.RawMessage(`{"command":"move"}`)); err != nil || refused == nil {
		t.Fatalf("a mutating call ran in plan mode: refused %v err %v", refused, err)
	}
	if _, refused, err := l.ManualAuthorize("mover", "m2", json.RawMessage(`{"command":"look"}`)); err != nil || refused != nil {
		t.Fatalf("a reading call was refused: %v %v", refused, err)
	}
}
