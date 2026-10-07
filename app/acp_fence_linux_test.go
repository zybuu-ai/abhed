package app

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// The Studio path, with git and the editor's files protected, runs a
// command under the fence in its mount mode, with a cgroup of the session's
// own: the command writes the workspace but not git's hooks. It needs what
// the fence's own tests need, and runs with ABHED_REQUIRE_FENCE=1.
func TestStudioRunsCommandsUnderTheFence(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 in a delegated cgroup to fence real commands")
	}
	r := newStudioRig(t, `,"sandbox":{"tier":"fence","max_procs":128,"max_memory_mb":512}`)
	for _, args := range [][]string{{"init", "-q"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	r.model.script(callTool("c1", "bash", map[string]any{"command": "echo hi > made.txt; echo x > .git/hooks/pre-commit; echo hooks=$?"}), say("done"))
	r.cl.answering(func(_ string, params json.RawMessage) any { return chosen(params, "allow_once") })
	id := r.open()
	r.prompt(id, "run it")

	q, _ := r.recorded(id, agent.EvFenceQualified)
	if len(q) != 1 || q[0]["mode"] != sandbox.FenceModeMounts {
		t.Fatalf("fence.qualified: %v", q)
	}
	l, _ := r.recorded(id, agent.EvProcessLaunched)
	if len(l) != 1 || l[0]["call_id"] != "c1" || l[0]["mode"] != sandbox.FenceModeMounts || l[0]["source"] != "call" {
		t.Fatalf("process.launched: %v", l)
	}
	cg, _ := l[0]["cgroup"].(string)
	sbx, _ := q[0]["sandbox"].(string)
	if sbx == "" || !strings.Contains(cg, sbx) {
		t.Errorf("the call's cgroup %q is not under the session's %q", cg, sbx)
	}
	obs, _ := r.recorded(id, agent.EvObservation)
	if len(obs) != 1 || !strings.Contains(obs[0]["content"].(string), "hooks=1") {
		t.Fatalf("observation: %v", obs)
	}
	if r.read("made.txt") != "hi\n" {
		t.Fatalf("made.txt: %q", r.read("made.txt"))
	}
	if _, err := os.Lstat(filepath.Join(r.ws, ".git", "hooks", "pre-commit")); err == nil {
		t.Fatal("a hook was written under the fence")
	}
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	if _, err := os.Stat(cg); err == nil {
		t.Errorf("the call's cgroup %s outlived the session", cg)
	}
}
