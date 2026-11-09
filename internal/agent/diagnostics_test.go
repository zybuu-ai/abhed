package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
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
	if d, ok, _ := l.EngineAuthorize(context.Background(), "bash", "d1", "diagnostics", args, plain); ok || d.Decision != policy.Deny {
		t.Fatalf("deny rule: %+v %v", d, ok)
	}
	other := json.RawMessage(`{"command":"basedpyright-langserver --stdio","description":"x"}`)
	if d, ok, _ := l.EngineAuthorize(context.Background(), "bash", "d2", "diagnostics", other, func(d policy.Result) bool { return d.Decision == policy.Allow }); ok || d.Decision != policy.Ask {
		t.Fatalf("an ask nobody answers: %+v %v", d, ok)
	}
	if d, ok, _ := l.EngineAuthorize(context.Background(), "bash", "d3", "diagnostics", other, plain); !ok || d.Step != "default" {
		t.Fatalf("an ask the setting answers: %+v %v", d, ok)
	}
	h := &refusingHooks{}
	l.Hooks = h
	d, ok, _ := l.EngineAuthorize(context.Background(), "bash", "d4", "diagnostics", other, plain)
	if ok || d.Decision != policy.Deny || d.Step != "hook" || len(h.asked) != 1 {
		t.Fatalf("a hook's refusal: %+v %v %v", d, ok, h.asked)
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
