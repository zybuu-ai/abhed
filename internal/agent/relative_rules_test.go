package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A loop reads relative path rules against its session's roots, the person's
// explorer actions included, and a subagent against its own roots as well.
func TestLoopReadsRelativePathRulesAgainstItsRoots(t *testing.T) {
	ws, extra := t.TempDir(), t.TempDir()
	sess, err := tools.NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AddRoot(extra); err != nil {
		t.Fatal(err)
	}
	pol := policy.New(policy.ModeBypass)
	if err := pol.AddDeny("write(docs/**/frozen/**)", "delete(apps/**/vault/**)", "rename(infra/**/pinned/**)", "read(notes/*)"); err != nil {
		t.Fatal(err)
	}
	pol.Roots = sess.PolicyRoots
	l := NewLoop(nil, tools.NewRegistry(tools.Bash{}, tools.Write{}), pol, nil, sess, NewRecorder(NewMemStore(), "s-rel", ""), DefaultConfig())
	path := func(p string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"path": p})
		return b
	}
	for _, c := range []struct{ tool, path string }{
		{"write", filepath.Join(ws, "docs/user guide/frozen/f.md")},
		{"delete", filepath.Join(ws, "apps/mobile/vault/s.txt")},
		{"rename", filepath.Join(ws, "infra/m/pinned/p.tf")},
		{"read", filepath.Join(extra, "notes/n.md")},
	} {
		if d := l.Policy.Evaluate(c.tool, c.tool != "read", path(c.path)); d.Decision != policy.Deny {
			t.Errorf("%s %s: %s, want deny", c.tool, c.path, d.Decision)
		}
	}
	for i, action := range []string{"delete", "rename"} {
		target := filepath.Join(ws, "apps/mobile/vault/s.txt")
		if action == "rename" {
			target = filepath.Join(ws, "infra/m/pinned/p.tf")
		}
		res, err := l.ManualAs(context.Background(), sess, action, "m"+string(rune('0'+i)), path(target), "bash", bashArgs("true"))
		if err != nil || !res.IsError {
			t.Errorf("explorer %s under a relative rule ran: %+v %v", action, res, err)
		}
	}

}

// A subagent spawned into its own worktree reads relative path rules against
// that worktree as well as the parent's roots.
func TestSpawnedSubagentReadsRelativeRulesInItsWorktree(t *testing.T) {
	child := tempDir(t)
	target := filepath.Join(child, "docs/x/frozen/f.md")
	f := subFactory(t, []scriptedTurn{
		{calls: []model.ToolCall{call("write", map[string]string{"path": target, "content": "x"})}},
		{text: "done"},
	}, NewBudget(1_000_000, 10, false))
	f.Policy = policy.New(policy.ModeBypass)
	f.Policy.Roots = f.Session.PolicyRoots
	if err := f.Policy.AddDeny("write(docs/**/frozen/**)"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Spawn(context.Background(), SubagentRequest{Prompt: "x", Description: "y", Workspace: child}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the subagent wrote under a relative deny rule in its worktree")
	}
	for _, r := range f.Policy.Roots() {
		if r == child {
			t.Fatal("spawning changed the parent's roots")
		}
	}
}
