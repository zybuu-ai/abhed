package toolset

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// Police hands the engine to the extensions' matchers, so a match rule
// written relative to the workspace takes the absolute paths the model sends.
func TestHookMatchUsesTheWorkspace(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "block.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\nwhile IFS= read -r line; do echo '{\"block\":true,\"reason\":\"no\"}'; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := extension.NewHost(nil)
	if errs := h.Load(context.Background(), []extension.Config{{Name: "guard", Command: "bash", Args: []string{script},
		Events: []extension.Event{extension.EvToolCall}, Match: []string{"write(src/**)"}, Timeout: 5 * time.Second}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	t.Cleanup(h.Close)
	pol := policy.New(policy.ModeAcceptEdits)
	pol.Roots = func() []string { return []string{ws} }
	Police(h, pol, "s")
	if got := pol.Evaluate("write", true, []byte(`{"path":"`+ws+`/src/a.go"}`)); got.Decision != policy.Deny {
		t.Fatalf("the relative match rule did not take the call: %+v", got)
	}
	if got := pol.Evaluate("write", true, []byte(`{"path":"`+ws+`/docs/a.md"}`)); got.Decision != policy.Allow {
		t.Fatalf("a call outside the match rule was screened: %+v", got)
	}
	// A copy of the engine with more roots, as a worktree subagent's is,
	// judges with its own.
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	child := *pol
	child.Roots = func() []string { return []string{wt, ws} }
	if got := child.Evaluate("write", true, []byte(`{"path":"`+wt+`/src/a.go"}`)); got.Decision != policy.Deny {
		t.Fatalf("the worktree's call skipped the hook: %+v", got)
	}
}
