package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/monitor"
)

type fakeDiagnoser struct {
	mu    sync.Mutex
	paths []string
}

func (f *fakeDiagnoser) Diagnose(_ context.Context, _ *Loop, _, path string) (Diagnosed, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, path)
	return Diagnosed{LSPDiagnostics: LSPDiagnostics{Path: path, Language: "go", Total: 1, Shown: 1, Errors: 1},
		Text: "[gopls diagnostics for a.go: 1 errors, 0 warnings, 1 in all]\na.go:1:1: error: undefined: x"}, true
}

func (f *fakeDiagnoser) Close() {}

// A successful write or edit is followed by the file's diagnostics, in the
// result the model reads and in the record; a failed edit gets none.
func TestDiagnosticsFollowAnEdit(t *testing.T) {
	turns := []scriptedTurn{
		{calls: []model.ToolCall{call("write", map[string]string{"path": "a.go", "content": "package a\n"})}},
		{calls: []model.ToolCall{{ID: "cbad", Name: "edit", Args: json.RawMessage(`{"path":"a.go","old_string":"missing","new_string":"x"}`)}}},
		{text: "done"},
	}
	l, store, dir := harness(t, turns, policy.ModeBypass, true)
	f := &fakeDiagnoser{}
	l.Diagnostics = f
	if _, err := l.Run(context.Background(), "write it"); err != nil {
		t.Fatal(err)
	}
	if len(f.paths) != 1 || f.paths[0] != filepath.Join(dir, "a.go") {
		t.Fatalf("diagnosed %v, want only the write's absolute path", f.paths)
	}
	evs, _ := store.Events("sess1")
	var diag []LSPDiagnostics
	sawText := false
	for _, e := range evs {
		switch e.Type {
		case EvLSPDiagnostics:
			var d LSPDiagnostics
			_ = json.Unmarshal(e.Payload, &d)
			diag = append(diag, d)
		case EvObservation:
			var o Observation
			_ = json.Unmarshal(e.Payload, &o)
			if o.CallID == "cwrite" && strings.Contains(o.Content, "undefined: x") {
				sawText = true
			}
			if o.CallID == "cbad" && strings.Contains(o.Content, "diagnostics") {
				t.Fatalf("a failed edit was given diagnostics: %s", o.Content)
			}
		}
	}
	if !sawText {
		t.Fatal("the write's result did not carry the diagnostics")
	}
	if len(diag) != 1 || diag[0].CallID != "cwrite" || diag[0].Errors != 1 {
		t.Fatalf("lsp.diagnostics %+v", diag)
	}
}

type refusingHooks struct{ asked []string }

func (h *refusingHooks) PromptSubmitted(context.Context, string, string) string { return "" }
func (h *refusingHooks) PermissionRequested(_ context.Context, _ *policy.Engine, _, tool string, _ json.RawMessage, reason string) string {
	h.asked = append(h.asked, tool+": "+reason)
	return "the organisation's hook refuses it"
}
func (h *refusingHooks) Observe(context.Context, string, string, string, string) {}

// The engine's own command is judged and recorded as a call: a deny rule
// refuses it; an ask runs only when allowed takes it as answered, and then
// the PermissionRequest hooks hear it first and may refuse it; what decided
// is on record as action.requested and action.approved or action.denied.
func TestEngineAuthorizeIsJudged(t *testing.T) {
	plain := func(d policy.Result) bool {
		return d.Decision == policy.Allow || d.Decision == policy.Ask && d.Step == "default"
	}
	l, store, _ := harness(t, nil, policy.ModeDefault, true)
	if err := l.Policy.AddDeny("bash(gopls*)"); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"command":"gopls","description":"x"}`)
	if v, _ := l.EngineAuthorize(context.Background(), "bash", "d1", DiagnosticsVia, args, plain); v.Allowed || v.Decision != policy.Deny {
		t.Fatalf("deny rule: %+v", v)
	}
	other := json.RawMessage(`{"command":"basedpyright-langserver --stdio","description":"x"}`)
	if v, _ := l.EngineAuthorize(context.Background(), "bash", "d2", DiagnosticsVia, other, func(d policy.Result) bool { return d.Decision == policy.Allow }); v.Allowed || v.Decision != policy.Ask {
		t.Fatalf("an ask nobody answers: %+v", v)
	}
	if v, _ := l.EngineAuthorize(context.Background(), "bash", "d3", DiagnosticsVia, other, plain); !v.Allowed || v.Step != "default" {
		t.Fatalf("an ask the setting answers: %+v", v)
	}
	h := &refusingHooks{}
	l.Hooks = h
	v, _ := l.EngineAuthorize(context.Background(), "bash", "d4", DiagnosticsVia, other, plain)
	if v.Allowed || v.Decision != policy.Deny || v.Step != "hook" || len(h.asked) != 1 {
		t.Fatalf("a hook's refusal: %+v %v", v, h.asked)
	}
	evs, _ := store.Events("sess1")
	got := map[string]string{}
	for _, e := range evs {
		var m map[string]any
		_ = json.Unmarshal(e.Payload, &m)
		id, _ := m["call_id"].(string)
		switch e.Type {
		case EvActionRequested:
			if m["via"] != "diagnostics" || e.Actor != ActorSystem {
				t.Fatalf("request %v by %s", m, e.Actor)
			}
			got[id] += "requested "
		case EvActionApproved:
			got[id] += "approved:" + m["by"].(string) + " "
		case EvActionDenied:
			got[id] += "denied:" + m["step"].(string) + " "
		}
	}
	want := map[string]string{"d1": "requested denied:deny ", "d2": "requested denied:default ", "d3": "requested approved:setting ", "d4": "requested denied:hook "}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("%s recorded %q, want %q (all %v)", id, got[id], w, got)
		}
	}
}

// A diagnostics server's start that a hard kill left with no outcome, in a
// turn before the last, is closed when the record is reconciled, once.
func TestReconcileClosesADiagnosticsStart(t *testing.T) {
	st := NewMemStore()
	p := NewRecorder(st, "p", "")
	_, _ = p.Record(EvUserMessage, ActorUser, Trusted, Message{Text: "go"})
	_, _ = p.Record(EvModelCall, ActorSystem, Trusted, ModelCall{Turn: 1})
	_, _ = p.Record(EvActionRequested, ActorSystem, Trusted, ActionRequested{CallID: "diag-go-1", Tool: "bash", Via: DiagnosticsVia, Args: mustJSON(map[string]string{"command": "gopls"})})
	_, _ = p.Record(EvActionApproved, ActorSystem, Trusted, map[string]string{"call_id": "diag-go-1", "by": ByPolicy})
	_, _ = p.Record(EvSessionEnded, ActorSystem, Trusted, SessionEnded{Reason: TermCompleted})
	_, _ = p.Record(EvUserMessage, ActorUser, Trusted, Message{Text: "more"})
	_, _ = p.Record(EvModelCall, ActorSystem, Trusted, ModelCall{Turn: 2})
	evs, _ := st.Events("p")
	if err := Reconcile(st, "p", evs); err != nil {
		t.Fatal(err)
	}
	evs, _ = st.Events("p")
	n := 0
	for _, e := range evs {
		var o Observation
		if e.Type == EvObservation && json.Unmarshal(e.Payload, &o) == nil && o.CallID == "diag-go-1" {
			n++
			if !o.IsError || !strings.Contains(o.Content, "not started") {
				t.Fatalf("outcome %+v", o)
			}
		}
	}
	if n != 1 {
		t.Fatalf("the start was closed %d times", n)
	}
	before := len(evs)
	if err := Reconcile(st, "p", evs); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.Events("p"); len(again) != before {
		t.Fatal("reconciled twice")
	}
}

// A diagnostics server's start after a passing test run does not put the
// run out of date: it changes nothing the agent did.
func TestDiagnosticsStartIsNoChangeToEvidence(t *testing.T) {
	b := newRec(t)
	b.call("c1", "bash", map[string]string{"command": "go test ./..."}, "ok", exit(0))
	raw, _ := json.Marshal(map[string]string{"command": "gopls"})
	_, _ = b.r.Record(EvActionRequested, ActorSystem, Trusted, ActionRequested{CallID: "diag-go-1", Tool: "bash", Args: raw, Via: DiagnosticsVia})
	_, _ = b.r.Record(EvObservation, ActorSystem, Trusted, Observation{CallID: "diag-go-1", Tool: "bash", Content: "language server gopls started for diagnostics (allowed by policy: bash(gopls))"})
	wantVerdict(t, checkText(t, b, "", "The tests pass now.")[0], ClaimVerified)
}

// The engine's start is reviewed but spends none of the turn's reviews, and
// a supervisor that cannot answer refuses it for this attempt only.
func TestEngineReviewIsUnbudgetedAndRetriable(t *testing.T) {
	l, _, _ := harness(t, nil, policy.ModeBypass, true)
	judged := 0
	l.Monitor = &monitor.Guard{MaxPerTurn: 1, Monitor: monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		judged++
		return monitor.Verdict{Decision: policy.Allow, Code: monitor.CodeOK}, nil
	})}
	args := json.RawMessage(`{"command":"gopls","description":"x"}`)
	all := func(d policy.Result) bool { return d.Decision == policy.Allow }
	for i := range 3 {
		if v, _ := l.EngineAuthorize(context.Background(), "bash", fmt.Sprint("d", i), DiagnosticsVia, args, all); !v.Allowed {
			t.Fatalf("start %d: %+v", i, v)
		}
	}
	if judged != 3 {
		t.Fatalf("judged %d of 3 starts: the turn's budget was spent", judged)
	}
	l.Monitor = &monitor.Guard{Monitor: monitor.Func(func(context.Context, monitor.Case) (monitor.Verdict, error) {
		return monitor.Verdict{}, errors.New("judge timed out")
	})}
	if v, _ := l.EngineAuthorize(context.Background(), "bash", "d9", DiagnosticsVia, args, all); v.Allowed || !v.Retry {
		t.Fatalf("an unavailable supervisor: %+v", v)
	}
}
