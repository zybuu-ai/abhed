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

// The engine's own command is judged as a call: a deny rule refuses it, and
// a plain ask stays an ask.
func TestEngineDecisionIsJudged(t *testing.T) {
	l, _, _ := harness(t, nil, policy.ModeDefault, true)
	if err := l.Policy.AddDeny("bash(gopls*)"); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"command":"gopls","description":"x"}`)
	if d := l.EngineDecision(context.Background(), "bash", "d1", args); d.Decision != policy.Deny {
		t.Fatalf("deny rule: %+v", d)
	}
	other := json.RawMessage(`{"command":"basedpyright-langserver --stdio","description":"x"}`)
	if d := l.EngineDecision(context.Background(), "bash", "d2", other); d.Decision != policy.Ask {
		t.Fatalf("default mode: %+v", d)
	}
}
