package policy

import (
	"os"
	"path/filepath"
	"testing"
)

// An offered git scope carries the repository's fingerprint, and a scope
// remembered before a hook was planted no longer covers the call.
func TestGitScopeIsGuardedByTheRepository(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := New(ModeDefault)
	e.Roots = func() []string { return []string{ws} }
	args := []byte(`{"command":"git status --short"}`)
	first := e.Evaluate("bash", true, args)
	if first.Offer() != "bash(git status *)" || first.Guard == "" {
		t.Fatalf("offer %q guard %q", first.Offer(), first.Guard)
	}
	if !e.Evaluate("bash", true, args).Covers(first.Offer(), first.Guard) {
		t.Fatal("an unchanged repository is not covered by its own grant")
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "config"), []byte("[core]\n\tfsmonitor = ./x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if e.Evaluate("bash", true, args).Covers(first.Offer(), first.Guard) {
		t.Fatal("a scope granted before fsmonitor was set still covers git status")
	}
	// Other scopes carry no guard.
	if r := e.Evaluate("bash", true, []byte(`{"command":"mkdir x"}`)); r.Offer() == "" || r.Guard != "" {
		t.Fatalf("mkdir: offer %q guard %q", r.Offer(), r.Guard)
	}
}
