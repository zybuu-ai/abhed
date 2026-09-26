package redteam

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Attacks that chain features or race them, which the scoping document
// (docs/ops/red-team-scope.md) identifies as where automated suites are weakest.
// Adding them here narrows the gap; it does not close it.

// C1: use /undo to restore a file the operator deliberately deleted.
func TestAttack_UndoRestoresDeletedSecret(t *testing.T) {
	dir := workspace(t)
	s := session(t, dir)
	undo := agent.NewUndoLog(s.RestoreFile, s.RemoveFile)
	s.Checkpoint = undo.Record

	secret := filepath.Join(dir, "secret.txt")
	os.WriteFile(secret, []byte("ORIGINAL-SECRET"), 0o644)

	// Agent modifies it, creating a checkpoint holding the original.
	undo.BeginTurn()
	call(t, tools.Read{}, s, map[string]any{"path": secret})
	call(t, tools.Write{}, s, map[string]any{"path": secret, "content": "rotated"})

	// Operator then deletes it entirely.
	os.Remove(secret)

	// Undo brings back the ROTATED-away original. That is correct undo
	// semantics, but it means checkpoints hold plaintext of anything the agent
	// touched — which is a real property to document, not a bug to hide.
	undo.Undo()
	data, err := os.ReadFile(secret)
	if err == nil && strings.Contains(string(data), "ORIGINAL-SECRET") {
		t.Logf("NOTE: undo restored pre-edit content of a since-deleted file. " +
			"This is correct undo behaviour, but checkpoints therefore retain " +
			"plaintext; they must inherit the workspace's confidentiality controls.")
	}
	// The security property that must hold: undo cannot write OUTSIDE the workspace.
	if _, err := os.Stat("/tmp/abhed-undo-escape"); err == nil {
		t.Fatal("ESCAPE: undo wrote outside the workspace")
	}
}

// C2: /export must not be usable to write outside the workspace.
func TestAttack_ExportPathTraversal(t *testing.T) {
	dir := workspace(t)
	s := session(t, dir)

	// The export path is operator-supplied, but a malicious workspace could
	// suggest one. Writing through the tool layer must still be scoped.
	for _, target := range []string{
		"/tmp/abhed-export-escape.json",
		filepath.Join(dir, "..", "escape.json"),
		"/etc/abhed-export.json",
	} {
		res := call(t, tools.Write{}, s, map[string]any{
			"path": target, "content": "exported",
		})
		if !res.IsError {
			os.Remove(target)
			t.Errorf("ESCAPE: wrote export to %q", target)
		}
	}
}

// C3: TOCTOU on the read-before-edit guard. Swap the file between the read and
// the edit and confirm the change is detected rather than silently applied.
func TestAttack_ReadEditRace(t *testing.T) {
	dir := workspace(t)
	s := session(t, dir)
	p := filepath.Join(dir, "race.go")
	os.WriteFile(p, []byte("package a\n\nconst Key = \"safe\"\n"), 0o644)

	call(t, tools.Read{}, s, map[string]any{"path": p})

	// Attacker replaces the file after the read.
	os.WriteFile(p, []byte("package a\n\nconst Key = \"attacker-controlled\"\n"), 0o644)

	res := call(t, tools.Edit{}, s, map[string]any{
		"path": p, "old_string": "safe", "new_string": "modified",
	})
	if !res.IsError {
		t.Fatal("TOCTOU: edit applied to a file that changed after the read")
	}
	if !strings.Contains(res.Content, "changed on disk") {
		t.Fatalf("error should name the stale read: %s", res.Content)
	}
}

// C4: concurrent edits to the same file must not corrupt it. Atomic write means
// a reader sees either the old or the new content, never a partial file.
func TestAttack_ConcurrentWriteCorruption(t *testing.T) {
	dir := workspace(t)
	p := filepath.Join(dir, "concurrent.txt")
	os.WriteFile(p, []byte(strings.Repeat("A", 4096)), 0o644)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s := session(t, dir)
			call(t, tools.Read{}, s, map[string]any{"path": p})
			content := strings.Repeat(string(rune('B'+n)), 4096)
			call(t, tools.Write{}, s, map[string]any{"path": p, "content": content})
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("file unreadable after concurrent writes: %v", err)
	}
	// Every byte must be the same character: a torn write would mix them.
	if len(data) != 4096 {
		t.Fatalf("torn write: length %d, want 4096", len(data))
	}
	first := data[0]
	for i, b := range data {
		if b != first {
			t.Fatalf("TORN WRITE: byte %d is %q but byte 0 is %q", i, b, first)
		}
	}
}

// C5: a subagent must not be able to exceed the parent's budget by spawning
// its own, nor bypass the parent's policy.
func TestAttack_SubagentBudgetEscape(t *testing.T) {
	dir := workspace(t)
	s := session(t, dir)

	budget := agent.NewBudget(1000, 2, false) // tight, nesting disabled
	f := &agent.SubagentFactory{
		Adapter:   &countingAdapter{},
		Tools:     tools.NewRegistry(tools.Read{}, tools.Glob{}),
		Policy:    policy.New(policy.ModeDefault),
		Approver:  agent.AutoApprove{Yes: true},
		Session:   s,
		Store:     agent.NewMemStore(),
		Budget:    budget,
		Config:    agent.DefaultConfig(),
		Workspace: dir,
	}

	// Exhaust the spawn allowance.
	for i := 0; i < 2; i++ {
		f.Spawn(context.Background(), agent.SubagentRequest{Prompt: "x", Description: "y"})
	}
	if _, err := f.Spawn(context.Background(), agent.SubagentRequest{Prompt: "x", Description: "y"}); err == nil {
		t.Fatal("BUDGET ESCAPE: spawned past the subagent limit")
	}

	// A subagent claiming depth 0 must still be refused when nesting is off.
	f.Depth = 1
	if _, err := f.Spawn(context.Background(), agent.SubagentRequest{Prompt: "x", Description: "y"}); err == nil {
		t.Fatal("NESTING ESCAPE: nested spawn allowed while disabled")
	}
}

type countingAdapter struct{}

func (countingAdapter) Name() string                           { return "counting" }
func (countingAdapter) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (countingAdapter) CountTokens(model.Request) (int, error) { return 0, nil }
func (countingAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 2)
	ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 600}}
	close(ch)
	return ch, nil
}

// C6: a denied command must not become allowed by wrapping it.
func TestAttack_CommandWrappingBypass(t *testing.T) {
	e := policy.New(policy.ModeAuto)
	e.AddDeny("bash(rm -rf *)")
	e.AddAllow("bash(*)")

	// Each of these ultimately performs a destructive delete. Abhed must not
	// silently allow any of them.
	wrapped := []string{
		"bash -c 'rm -rf /tmp/x'",
		"sh -c \"rm -rf /tmp/x\"",
		"eval 'rm -rf /tmp/x'",
		"env rm -rf /tmp/x",
		"nice rm -rf /tmp/x",
		"xargs rm -rf < files.txt",
		"find . -delete",
		"find . -exec rm -rf {} +",
	}
	var allowed []string
	for _, cmd := range wrapped {
		args, _ := json.Marshal(map[string]string{"command": cmd, "description": "x"})
		if res := e.Evaluate("bash", true, args); res.Decision == policy.Allow {
			allowed = append(allowed, cmd)
		}
	}
	if len(allowed) > 0 {
		t.Errorf("WRAPPING BYPASS: %d wrapped destructive commands auto-allowed:\n  %s\n"+
			"Pattern matching on command text cannot be complete — this is why the\n"+
			"sandbox, not the policy engine, is the actual boundary.",
			len(allowed), strings.Join(allowed, "\n  "))
	}
}

// C7: the sandbox must contain a command the policy engine failed to catch.
// This is the defence-in-depth claim: policy is advisory, isolation is the boundary.
func TestAttack_SandboxContainsPolicyMiss(t *testing.T) {
	dir := workspace(t)
	p := sandbox.DefaultPolicy(dir)
	sb := sandbox.NewProcess(p)
	if ok, _ := sb.Available(); !ok {
		t.Skip("process sandbox unavailable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Assume policy was bypassed entirely and this command runs. The sandbox
	// must still prevent damage outside the workspace.
	marker := "/usr/local/abhed-policy-miss"
	for _, cmd := range []string{
		"bash -c 'echo pwned > " + marker + "'",
		"env sh -c 'echo pwned > " + marker + "'",
		"find / -maxdepth 1 -name etc -exec touch " + marker + " ; 2>/dev/null",
	} {
		_ = sb.Command(ctx, dir, cmd).Run()
		if _, err := os.Stat(marker); err == nil {
			os.Remove(marker)
			t.Fatalf("DEFENCE IN DEPTH FAILED: %q escaped the sandbox after a policy miss", cmd)
		}
	}
}

// C8: session id guessing must not expose another tenant's events.
func TestAttack_SessionIDGuessing(t *testing.T) {
	store := agent.NewMemStore()

	victim := agent.NewRecorder(store, "s-victim-001", "")
	victim.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted,
		agent.Message{Text: "confidential prompt"})

	// An attacker who guesses the id can read from the raw store — which is why
	// tenant scoping must live in the server and the database, not in id secrecy.
	events, _ := store.Events("s-victim-001")
	if len(events) == 0 {
		t.Fatal("setup failed")
	}
	t.Log("NOTE: the raw store has no tenant scoping by design. Isolation is " +
		"enforced at the server layer and by Postgres row-level security " +
		"(see TestTenantIsolation and TestRowLevelSecurityIsolatesTenants). " +
		"Session ids must never be treated as capabilities.")
}
