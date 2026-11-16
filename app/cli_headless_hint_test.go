package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// hintStub is a model that makes one tool call, then answers.
func hintStub(t *testing.T, name, args string) string {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if n.Add(1) == 1 {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"`+
				name+`","arguments":`+strconv.Quote(args)+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A -p run's refused call says, on stderr and in the result, the exact flag
// that would allow it, or, for a deny rule, that no flag can.
func TestHeadlessDenialSuggestsTheFlag(t *testing.T) {
	for _, c := range []struct {
		name, tool, args, extra string
		flag, hint              string
	}{
		{"edit", "write", `{"path":"hello.txt","content":"hi"}`, "", "-mode accept-edits", "rerun with -mode accept-edits"},
		{"command", "bash", `{"command":"make deploy"}`, "", "-allow 'bash(make deploy)'", "rerun with -allow 'bash(make deploy)'"},
		{"deny rule", "bash", `{"command":"curl -sI https://example.com"}`, `,"permissions":{"deny":["bash(curl*)"]}`, "",
			"no flag lifts a deny rule; remove bash(curl*) from your user configuration ~/.abhed/config.json to allow this"},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := hintStub(t, c.tool, c.args)
			home, ws := t.TempDir(), t.TempDir()
			cfg := `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
				url + `","model":"m","context_window":8192}}}` + c.extra + `}`
			if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := mainHelper([]string{"-C", ws, "-p", "do it", "-output-format", "json"},
				"HOME="+home, "XDG_CONFIG_HOME="+t.TempDir(), "ABHED_SECRETS_FILE=", config.TrustEnv+"=")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			_ = cmd.Run()
			if !strings.Contains(stderr.String(), c.hint) {
				t.Fatalf("stderr lacks %q:\n%s", c.hint, stderr.String())
			}
			var res resultLine
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil {
				t.Fatalf("no result line: %v\n%s", err, stdout.String())
			}
			if len(res.PermissionDenials) != 1 {
				t.Fatalf("permission_denials: %+v\n%s", res.PermissionDenials, stdout.String())
			}
			d := res.PermissionDenials[0]
			if d.Tool != c.tool || d.Flag != c.flag || !strings.Contains(d.Hint, c.hint) {
				t.Fatalf("denial %+v; want flag %q, hint %q", d, c.flag, c.hint)
			}
			if _, err := os.Stat(filepath.Join(ws, "hello.txt")); err == nil {
				t.Fatal("explaining a denial granted it")
			}
		})
	}
}

// A flag the policy or the configuration would still refuse is never
// offered: an org or managed rule says to ask the admin, and a managed lock
// on the permissions or the mode leaves no flag.
func TestHeadlessHintNeverOffersARefusedFlag(t *testing.T) {
	ask := func(tool, args string) agent.Event {
		b, _ := json.Marshal(agent.ActionRequested{CallID: "c1", Tool: tool, Args: json.RawMessage(args)})
		return agent.Event{Type: agent.EvActionRequested, Payload: b}
	}
	denied := func(d map[string]string) agent.Event {
		d["call_id"] = "c1"
		b, _ := json.Marshal(d)
		return agent.Event{Type: agent.EvActionDenied, Payload: b}
	}
	registry := tools.NewRegistry(tools.Bash{}, tools.Write{})
	run := func(pol *policy.Engine, cfg config.Config, tool, args string, d map[string]string) permissionDenial {
		t.Helper()
		h := newDenialHints(pol, cfg, registry)
		h.observe(ask(tool, args))
		out, ok := h.observe(denied(d))
		if !ok {
			t.Fatal("no denial reported")
		}
		return out
	}
	pol := policy.New(policy.ModeDefault)
	if err := pol.AddDeny("bash(curl*)"); err != nil {
		t.Fatal(err)
	}
	for source, want := range map[string]string{"org": "denied by org policy; ask your admin", "managed": "denied by the managed configuration; ask your admin"} {
		got := run(pol, config.Default(), "bash", `{"command":"curl x"}`,
			map[string]string{"step": "deny", "rule": "bash(curl*)", "source": source, "by": "policy", "reason": "denied by rule bash(curl*)"})
		if got.Flag != "" || got.Hint != want {
			t.Fatalf("%s: %+v", source, got)
		}
	}

	// An MCP tool is allowed by its name.
	mcpTool := fakeTool{name: "mcp__tickets__get_ticket"}
	h := newDenialHints(policy.New(policy.ModeAcceptEdits), config.Default(), tools.NewRegistry(mcpTool))
	h.observe(ask(mcpTool.name, `{"id":"T-1"}`))
	if got, _ := h.observe(denied(map[string]string{"step": "default", "source": "mode", "by": "headless", "reason": "can make changes"})); got.Flag != "-allow 'mcp__tickets__get_ticket'" {
		t.Fatalf("MCP tool: %+v", got)
	}

	// An ask rule asks in every mode, so an allow flag would not help.
	if err := pol.AddAsk("bash(make deploy*)"); err != nil {
		t.Fatal(err)
	}
	got := run(pol, config.Default(), "bash", `{"command":"make deploy"}`,
		map[string]string{"step": "ask", "rule": "bash(make deploy*)", "source": "org", "by": "headless", "reason": "matched ask rule"})
	if got.Flag != "" || !strings.Contains(got.Hint, "ask your admin") {
		t.Fatalf("org ask rule: %+v", got)
	}

	// A built-in ask rule is not called the person's.
	got = run(pol, config.Default(), "bash", `{"command":"make deploy"}`,
		map[string]string{"step": "ask", "rule": "bash(make deploy*)", "source": "default", "by": "headless", "reason": "matched ask rule"})
	if got.Flag != "" || strings.Contains(got.Hint, "your") || !strings.Contains(got.Hint, "Abhed's built-in ask rule bash(make deploy*)") {
		t.Fatalf("built-in ask rule: %+v", got)
	}

	// Under a managed file that sets the permissions, -allow is refused.
	home := t.TempDir()
	t.Setenv("HOME", home)
	withManagedFile(t, `{"permissions":{"mode":"default"}}`)
	locked, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got = run(policy.New(policy.ModeDefault), locked, "write", `{"path":"a.txt","content":"x"}`,
		map[string]string{"step": "default", "source": "mode", "by": "headless", "reason": "changing a file needs approval in default mode"})
	if got.Flag != "" || !strings.Contains(got.Hint, "ask your admin") {
		t.Fatalf("a flag the managed configuration refuses was offered: %+v", got)
	}
}

// withManagedFile points the managed configuration at body for one test.
func withManagedFile(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
}

// fakeTool is a tool that changes something, named as an MCP tool is.
type fakeTool struct{ name string }

func (f fakeTool) Name() string          { return f.name }
func (fakeTool) Description() string     { return "a fake" }
func (fakeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (fakeTool) Mutates() bool           { return true }
func (fakeTool) Run(context.Context, *tools.Session, json.RawMessage) tools.Result {
	return tools.Result{}
}

// The engine's own start of a language server for diagnostics is no call of
// the agent's: refused, it is not a permission denial, and the -p run says
// once why there are no diagnostics, as a note.
func TestHeadlessDiagnosticsStartIsNoDenial(t *testing.T) {
	req, _ := json.Marshal(agent.ActionRequested{CallID: "diag-python-1", Tool: "bash", Via: agent.DiagnosticsVia,
		Args: json.RawMessage(`{"command":"basedpyright-langserver --stdio"}`)})
	den, _ := json.Marshal(map[string]string{"call_id": "diag-python-1", "step": "default", "by": "policy", "reason": "nobody is asked for it: can make changes"})
	h := newDenialHints(policy.New(policy.ModeAcceptEdits), config.Default(), tools.NewRegistry(tools.Bash{}))
	h.observe(agent.Event{Type: agent.EvActionRequested, Payload: req})
	if d, ok := h.observe(agent.Event{Type: agent.EvActionDenied, Payload: den}); ok || len(h.denials) != 0 {
		t.Fatalf("an engine start was reported as a denial: %+v", d)
	}
	skipped, _ := json.Marshal(agent.LSPDiagnostics{Language: "python", Path: "a.py",
		Skipped: "no python diagnostics: basedpyright-langserver is not installed where commands run; install it with: abhed toolchain install -lsp python"})
	if got := diagnosticsNote(agent.Event{Type: agent.EvLSPDiagnostics, Payload: skipped}); got != "note: no python diagnostics: basedpyright-langserver is not installed where commands run; install it with: abhed toolchain install -lsp python" {
		t.Fatalf("note %q", got)
	}
	refused, _ := json.Marshal(agent.LSPDiagnostics{Language: "python", Skipped: "not started: deny at deny (denied by rule bash(basedpyright*))"})
	if got := diagnosticsNote(agent.Event{Type: agent.EvLSPDiagnostics, Payload: refused}); got != "note: no python diagnostics: not started: deny at deny (denied by rule bash(basedpyright*))" {
		t.Fatalf("note %q", got)
	}
	ran, _ := json.Marshal(agent.LSPDiagnostics{Language: "python", Total: 2})
	if got := diagnosticsNote(agent.Event{Type: agent.EvLSPDiagnostics, Payload: ran}); got != "" {
		t.Fatalf("note for diagnostics given: %q", got)
	}
}
