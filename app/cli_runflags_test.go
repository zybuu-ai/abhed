package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// TestRunFlagsHelper runs abhed with the arguments a test below passes.
func TestRunFlagsHelper(t *testing.T) {
	raw := os.Getenv("ABHED_RUNFLAGS_ARGS")
	if raw == "" {
		t.Skip("run by TestRunFlagsEndToEnd")
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}
	os.Exit(Main(args))
}

// bodyModel answers "done" to every request and keeps each request's body.
func bodyModel(t *testing.T) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(bodies) }
}

// runFlagsMain runs abhed in a child process with args and a home of its own.
func runFlagsMain(t *testing.T, args ...string) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunFlagsHelper$")
	cmd.Env = append(os.Environ(), "ABHED_RUNFLAGS_ARGS="+string(raw), "HOME="+t.TempDir(), config.TrustEnv+"=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func startedPayload(t *testing.T, out string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		var ev agent.Event
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == agent.EvSessionStarted {
			var p map[string]any
			_ = json.Unmarshal(ev.Payload, &p)
			return p
		}
	}
	t.Fatalf("no session.started in:\n%s", out)
	return nil
}

// End to end: the provider comes from -settings, the session runs as the
// -agent role from -agents (its instructions, only its tools, plan mode),
// and the start event records each source by hash.
func TestRunFlagsEndToEnd(t *testing.T) {
	url, bodies := bodyModel(t)
	ws, dir := t.TempDir(), t.TempDir()
	settings := filepath.Join(dir, "ci.json")
	write(t, settings, `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"`+url+`","model":"m","context_window":8192}}}}`)
	mcpFile := filepath.Join(dir, "mcp.json")
	write(t, mcpFile, `{"mcpServers":{"absent":{"command":"/nonexistent/mcp-server"}}}`)
	agents := `{"auditor":{"description":"Audits code","prompt":"ROLE-MARK audit only.","tools":["read","grep"],"permissionMode":"plan"}}`

	out, err := runFlagsMain(t, "-C", ws, "-settings", settings, "-mcp-config", mcpFile, "-strict-mcp-config",
		"-agents", agents, "-agent", "auditor", "-p", "go", "-output-format", "json")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := bodies()
	if len(got) == 0 {
		t.Fatalf("the model was never asked:\n%s", out)
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(got[0]), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) == 0 || !strings.Contains(req.Messages[0].Content, "ROLE-MARK") {
		t.Fatalf("the role's instructions are not in the system prompt")
	}
	var names []string
	for _, tl := range req.Tools {
		names = append(names, tl.Function.Name)
	}
	if !slices.Contains(names, "read") || slices.Contains(names, "bash") || slices.Contains(names, "write") || slices.Contains(names, "task") {
		t.Fatalf("the session's tools: %v", names)
	}
	p := startedPayload(t, out)
	if p["mode"] != "plan" || p["strict_mcp_config"] != true {
		t.Fatalf("started: %v", p)
	}
	for _, k := range []string{"settings", "mcp_config", "agents", "agent"} {
		if p[k] == nil {
			t.Fatalf("started lacks %s: %v", k, p)
		}
	}
	if s, _ := p["settings"].(map[string]any); s["source"] != settings || len(s["sha256"].(string)) != 64 {
		t.Fatalf("settings recorded as %v", p["settings"])
	}
}

// A refused -agents definition, an unknown -agent and a bad -mcp-config end
// the run before the model is asked.
func TestRunFlagsRefusals(t *testing.T) {
	url, bodies := bodyModel(t)
	settings := `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + url + `","model":"m","context_window":8192}}}}`
	for _, args := range [][]string{
		{"-agents", `{"wide":{"description":"d","prompt":"p","permissionMode":"bypassPermissions"}}`},
		{"-agent", "nobody"},
		{"-mcp-config", `{"mcpServers":{"x":{"command":"a","url":"https://b"}}}`},
		{"-mcp-config", `{"servers":[]}`},
		{"-settings", filepath.Join(t.TempDir(), "missing.json")},
	} {
		full := append([]string{"-C", t.TempDir(), "-settings", settings}, args...)
		if args[0] == "-settings" {
			full = append([]string{"-C", t.TempDir()}, args...)
		}
		out, err := runFlagsMain(t, append(full, "-p", "go")...)
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if n := len(bodies()); n != 0 {
		t.Fatalf("the model was asked %d times", n)
	}
}

// -mcp-config reads both file shapes, enabling each server unless it says
// otherwise, and refuses what it cannot take.
func TestParseMCPConfig(t *testing.T) {
	got, err := parseMCPConfig([]byte(`{"mcpServers":{"gh":{"command":"npx","args":["-y","gh"],"env":{"B":"2","A":"1"}},"web":{"type":"http","url":"https://h/mcp","headers":{"X":"y"}}}}`), "f")
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if g := got[0]; g.Name != "gh" || !g.Enabled || g.Command != "npx" || strings.Join(g.Env, ",") != "A=1,B=2" {
		t.Fatalf("gh %+v", g)
	}
	if w := got[1]; w.URL != "https://h/mcp" || w.Headers["X"] != "y" || !w.Enabled {
		t.Fatalf("web %+v", w)
	}
	got, err = parseMCPConfig([]byte(`{"mcp":{"servers":[{"name":"a","command":"x"},{"name":"b","command":"y","enabled":false}]}}`), "f")
	if err != nil || !got[0].Enabled || got[1].Enabled {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{
		`{"other":{}}`, `{"mcpServers":{"a b":{"command":"x"}}}`, `{"mcpServers":{"a":{}}}`,
		`{"mcpServers":{"a":{"command":"x","oauth":{}}}}`, `{"mcpServers":{"a":{"type":"ws","url":"wss://h"}}}`, `[]`,
	} {
		if _, err := parseMCPConfig([]byte(bad), "f"); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}

// -mcp-config adds servers, or with -strict-mcp-config is the only source;
// a managed mcp section refuses added servers and keeps its own under strict.
func TestMCPFlagsAndManaged(t *testing.T) {
	managedConfig(t, "")
	cfg := config.Default()
	cfg.MCP.Servers = []config.MCPServerConfig{{Name: "mine", Command: "/bin/mine", Enabled: true}, {Name: "gh", Command: "/bin/old"}}
	file := `{"mcpServers":{"gh":{"command":"/bin/new"}}}`
	got, src, err := mcpFlags(cfg, flagFiles{}, []string{file}, false)
	if err != nil || len(got.MCP.Servers) != 2 || len(src) != 1 || src[0].Servers[0] != "gh" {
		t.Fatalf("%+v %v %v", got.MCP, src, err)
	}
	if s := got.MCP.Servers[1]; s.Name != "gh" || s.Command != "/bin/new" || !s.Enabled {
		t.Fatalf("the flag's gh did not replace the configured one: %+v", got.MCP.Servers)
	}
	if got, _, _ = mcpFlags(cfg, flagFiles{}, []string{file}, true); len(got.MCP.Servers) != 1 {
		t.Fatalf("strict kept %+v", got.MCP.Servers)
	}
	if got, _, _ = mcpFlags(cfg, flagFiles{}, nil, true); len(got.MCP.Servers) != 0 {
		t.Fatalf("strict with no file kept %+v", got.MCP.Servers)
	}

	managedConfig(t, `{"mcp":{"servers":[{"name":"org","command":"/bin/org","enabled":true}]}}`)
	mcfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var me *config.ManagedError
	if _, _, err := mcpFlags(mcfg, flagFiles{}, []string{file}, false); !errors.As(err, &me) {
		t.Fatalf("an added server under a managed list: %v", err)
	}
	if got, _, err := mcpFlags(mcfg, flagFiles{}, nil, true); err != nil || len(got.MCP.Servers) != 1 || got.MCP.Servers[0].Name != "org" {
		t.Fatalf("strict under a managed list: %+v %v", got.MCP.Servers, err)
	}
}

// -agent finds a role among every definition, and refuses a worktree role
// and a name nobody defines.
func TestRoleFor(t *testing.T) {
	managedConfig(t, "")
	cfg := config.Default()
	defs, src, err := sessionAgents(cfg, flagFiles{}, `{"iso":{"description":"d","prompt":"p","isolation":"worktree"},"plain":{"description":"d","prompt":"p"}}`)
	if err != nil || len(defs) != 2 || src == nil {
		t.Fatalf("%v %v", defs, err)
	}
	if d, err := roleFor(cfg, defs, "plain"); err != nil || d.Source != agent.SourceSession {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := roleFor(cfg, defs, "iso"); err == nil {
		t.Fatal("a worktree role ran as the session")
	}
	if _, err := roleFor(cfg, defs, "nobody"); err == nil || !strings.Contains(err.Error(), "plain") {
		t.Fatalf("unknown role: %v", err)
	}
	if d, err := roleFor(cfg, nil, "explore"); err != nil || d.Source != agent.SourceBuiltin {
		t.Fatalf("a built-in role: %+v %v", d, err)
	}
}

// A flag file inside the workspace is read only with workspace trust; one
// outside it, or inline JSON, needs none.
func TestFlagFileInsideWorkspaceNeedsTrust(t *testing.T) {
	t.Setenv(config.TrustEnv, "")
	t.Setenv("HOME", t.TempDir())
	ws, out := t.TempDir(), t.TempDir()
	write(t, filepath.Join(ws, "ci.json"), `{}`)
	write(t, filepath.Join(out, "ci.json"), `{}`)
	ff := newFlagFiles(ws, config.TrustAsStored)
	if _, _, err := ff.read("-settings", filepath.Join(ws, "ci.json")); err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("a workspace file was read untrusted: %v", err)
	}
	if _, _, err := ff.read("-settings", filepath.Join(out, "ci.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ff.read("-settings", `{"memory":{}}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newFlagFiles(ws, config.TrustGranted).read("-settings", filepath.Join(ws, "ci.json")); err != nil {
		t.Fatalf("-trust-workspace did not admit it: %v", err)
	}
	if _, _, err := ff.read("-settings", ws); err == nil {
		t.Fatal("a directory was read")
	}
}
