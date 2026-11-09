package toolset

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// hookSet builds a tool set whose configuration holds hooks, and puts its
// veto on a fresh engine.
func hookSet(t *testing.T, cfg config.Config, bash tools.Bash) *policy.Engine {
	t.Helper()
	s := Build(context.Background(), cfg, Options{Workspace: t.TempDir(), Parts: Vetoes, Bash: bash})
	t.Cleanup(s.Close)
	pol := policy.New(policy.ModeAuto)
	Police(s.Extensions, pol, "s")
	return pol
}

// A command hook from the configuration runs in the sandbox bash's commands
// run in, in the workspace, gets the event on stdin, and blocks with exit 2.
func TestConfiguredCommandHookRunsInTheSandbox(t *testing.T) {
	var sandboxed atomic.Int32
	bash := tools.Bash{Sandbox: func(ctx context.Context, cwd, command string) *exec.Cmd {
		sandboxed.Add(1)
		cmd := exec.CommandContext(ctx, "bash", "-c", command)
		cmd.Dir = cwd
		return cmd
	}}
	cfg := config.Default()
	cfg.Hooks.Run = []config.HookConfig{{Name: "no-push", Events: []string{"tool_call"}, Match: []string{"bash(git push*)"},
		Command: `ev=$(cat); case "$ev" in *'"tool":"bash"'*'git push'*) echo "pushes go through review" >&2; exit 2;; esac`}}
	pol := hookSet(t, cfg, bash)
	got := pol.EvaluateContext(context.Background(), "bash", true, json.RawMessage(`{"command":"git push origin main"}`))
	if got.Decision != policy.Deny || !strings.Contains(got.Reason, "pushes go through review") {
		t.Fatalf("the hook did not block: %+v", got)
	}
	if sandboxed.Load() != 1 {
		t.Fatalf("the hook ran %d times through the sandbox", sandboxed.Load())
	}
	if got := pol.EvaluateContext(context.Background(), "bash", true, json.RawMessage(`{"command":"git status"}`)); got.Decision == policy.Deny || sandboxed.Load() != 1 {
		t.Fatalf("an unmatched call was screened: %+v", got)
	}
}

// Without a sandbox the command runs as bash's would, and still decides.
func TestCommandHookWithoutASandbox(t *testing.T) {
	run := hookRunner(tools.Bash{}, t.TempDir())
	res, err := run(context.Background(), "", `cat; echo nope >&2; exit 2`, []byte(`{"event":"stop"}`))
	if err != nil || res.Exit != 2 || string(res.Stdout) != `{"event":"stop"}` || strings.TrimSpace(string(res.Stderr)) != "nope" {
		t.Fatalf("%+v %v", res, err)
	}
}

// An HTTP hook's request goes through the set's egress guard: under the
// allowlist an endpoint it does not name is refused, and the call with it.
func TestHTTPHookIsUnderTheAllowlist(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	port := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://")).Port()
	for _, allowed := range []bool{false, true} {
		cfg := config.Default()
		cfg.Sandbox.Network = config.NetworkAllowlist
		if allowed {
			cfg.Egress.Rules = []egress.Rule{{Host: "127.0.0.1", Ports: []int{int(port)}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}
		}
		cfg.Hooks.Run = []config.HookConfig{{Name: "audit", Events: []string{"tool_call"}, URL: srv.URL + "/hook"}}
		pol := hookSet(t, cfg, tools.Bash{})
		before := hits.Load()
		got := pol.EvaluateContext(context.Background(), "read", false, json.RawMessage(`{"path":"a"}`))
		if allowed && (got.Decision != policy.Allow || hits.Load() != before+1) {
			t.Fatalf("an allowed endpoint: %+v, %d hits", got, hits.Load()-before)
		}
		if !allowed && (got.Decision != policy.Deny || hits.Load() != before) {
			t.Fatalf("an endpoint off the allowlist: %+v, %d hits", got, hits.Load()-before)
		}
	}
}

// hooks.disabled leaves no simple hook, and hooks.managed_only only the
// managed file's.
func TestHookSpecsHonourTheManagedSwitches(t *testing.T) {
	cfg := config.Default()
	cfg.Hooks.Run = []config.HookConfig{
		{Name: "mine", Events: []string{"stop"}, Command: "true", Layer: config.LayerUser},
		{Name: "org", Events: []string{"stop"}, Command: "true", Layer: config.LayerManaged},
	}
	if n := len(cfg.HookSpecs()); n != 2 {
		t.Fatalf("%d hooks", n)
	}
	cfg.Hooks.ManagedOnly = true
	if s := cfg.HookSpecs(); len(s) != 1 || s[0].Name != "org" {
		t.Fatalf("managed only: %+v", s)
	}
	cfg.Hooks.Disabled = true
	if s := cfg.HookSpecs(); len(s) != 0 {
		t.Fatalf("disabled: %+v", s)
	}
	set := Build(context.Background(), cfg, Options{Workspace: t.TempDir(), Parts: Vetoes})
	t.Cleanup(set.Close)
	if set.Extensions.Len() != 0 {
		t.Fatal("a disabled hook was loaded")
	}
}

// A command hook runs in the folder the session works in, as its commands do.
func TestCommandHookRunsInTheSessionsFolder(t *testing.T) {
	ws, tree := t.TempDir(), t.TempDir()
	run := hookRunner(tools.Bash{}, ws)
	res, err := run(context.Background(), tree, `pwd -P`, nil)
	real, _ := filepath.EvalSymlinks(tree)
	if err != nil || strings.TrimSpace(string(res.Stdout)) != real {
		t.Fatalf("ran in %q, want %q (%v)", res.Stdout, real, err)
	}
}

// A simple hook on tool_result is sent each result through the loop's hooks.
func TestToolResultHookIsSent(t *testing.T) {
	cfg := config.Default()
	cfg.Hooks.Run = []config.HookConfig{{Name: "seen", Events: []string{"tool_result"},
		Command: `cat > "$HOOK_OUT"`}}
	out := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("HOOK_OUT", out)
	set := Build(context.Background(), cfg, Options{Workspace: t.TempDir(), Parts: Vetoes})
	t.Cleanup(set.Close)
	hooks := HostHooks{Host: set.Extensions}
	content, isErr := hooks.ToolResult(context.Background(), "s1", "read", "file body", false)
	if content != "file body" || isErr {
		t.Fatalf("a simple hook changed the result: %q %v", content, isErr)
	}
	data, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(data), `"event":"tool_result"`) || !strings.Contains(string(data), "file body") {
		t.Fatalf("the hook was sent %s (%v)", data, err)
	}
}
