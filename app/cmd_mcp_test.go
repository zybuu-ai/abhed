package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// mcpRun runs `abhed mcp` with answer typed at a terminal, or with no
// terminal when tty is false.
func mcpRun(t *testing.T, tty bool, answer string, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := mcpCmd(t.TempDir(), args, config.TrustAsStored, mcpIO{in: strings.NewReader(answer), out: &out, err: &out, terminal: tty})
	return code, out.String()
}

func userConfigDoc(t *testing.T) map[string]any {
	t.Helper()
	home, _ := os.UserHomeDir()
	doc := map[string]any{}
	data, err := os.ReadFile(filepath.Join(home, ".abhed", "config.json"))
	if os.IsNotExist(err) {
		return doc
	}
	if err != nil || json.Unmarshal(data, &doc) != nil {
		t.Fatalf("user config: %v %s", err, data)
	}
	return doc
}

func changeLog(t *testing.T) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	data, _ := os.ReadFile(filepath.Join(home, ".abhed", "config-changes.jsonl"))
	return string(data)
}

// add confirms at a terminal, writes the server enabled into the person's
// own file beside what was there, and logs the change without its values.
func TestMCPAddConfirmsWritesAndLogs(t *testing.T) {
	managedConfig(t, "")
	home, _ := os.UserHomeDir()
	write(t, filepath.Join(home, ".abhed", "config.json"), `{"memory":{"auto":true}}`)
	code, out := mcpRun(t, true, "y\n", "add", "-env", "TOKEN=SECRET-CANARY", "-env", "GITHUB_TOKEN", "-allow-tools", "read_doc", "docs", "/usr/bin/docs-mcp", "--stdio")
	if code != 0 || !strings.Contains(out, "docs-mcp --stdio") || strings.Contains(out, "SECRET-CANARY") || !strings.Contains(out, "GITHUB_TOKEN (your own value)") {
		t.Fatalf("code %d: %s", code, out)
	}
	cfg, err := userFileConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCP.Servers) != 1 || !cfg.Memory.Auto {
		t.Fatalf("written: %+v %+v", cfg.MCP, cfg.Memory)
	}
	s := cfg.MCP.Servers[0]
	if s.Name != "docs" || !s.Enabled || s.Command != "/usr/bin/docs-mcp" || s.Args[0] != "--stdio" || s.AllowTools[0] != "read_doc" {
		t.Fatalf("server %+v", s)
	}
	log := changeLog(t)
	if !strings.Contains(log, `"by":"mcp add"`) || !strings.Contains(log, `"name":"docs"`) || strings.Contains(log, "SECRET-CANARY") {
		t.Fatalf("change log: %s", log)
	}

	// The same name again is refused.
	if code, _ := mcpRun(t, true, "y\n", "add", "docs", "/bin/other"); code == 0 {
		t.Fatal("a second server took the same name")
	}

	if code, out := mcpRun(t, false, "", "remove", "docs"); code != 0 {
		t.Fatalf("remove: %s", out)
	}
	if cfg, _ := userFileConfig(); len(cfg.MCP.Servers) != 0 || !cfg.Memory.Auto {
		t.Fatalf("after remove: %+v", cfg)
	}
	if !strings.Contains(changeLog(t), `"by":"mcp remove"`) {
		t.Fatal("the removal was not logged")
	}
}

// Without a terminal, or without a yes, nothing is added.
func TestMCPAddNeedsAYesAtATerminal(t *testing.T) {
	managedConfig(t, "")
	for _, c := range []struct {
		tty    bool
		answer string
	}{{false, "y\n"}, {true, "n\n"}, {true, ""}} {
		mcpRun(t, c.tty, c.answer, "add", "remote", "https://mcp.example.com/mcp")
		if servers := userServers(userConfigDoc(t)); len(servers) != 0 || changeLog(t) != "" {
			t.Fatalf("tty %v answer %q added %v", c.tty, c.answer, servers)
		}
	}
}

// Inside an agent's command, which sets ABHED_SANDBOX, add is refused even
// with a yes at what looks like a terminal.
func TestMCPAddRefusedInsideTheSandbox(t *testing.T) {
	managedConfig(t, "")
	var out bytes.Buffer
	code := mcpCmd(t.TempDir(), []string{"add", "remote", "https://mcp.example.com/mcp"}, config.TrustAsStored,
		mcpIO{in: strings.NewReader("y\n"), out: &out, err: &out, terminal: true, inSandbox: true})
	if servers := userServers(userConfigDoc(t)); code == 0 || len(servers) != 0 || changeLog(t) != "" {
		t.Fatalf("add inside the sandbox: code %d, servers %v, %s", code, servers, out.String())
	}
	if !strings.Contains(out.String(), "ABHED_SANDBOX") {
		t.Fatalf("the refusal does not say why: %s", out.String())
	}
	// With no terminal either, the refusal still names the sandbox, the real cause.
	out.Reset()
	mcpCmd(t.TempDir(), []string{"add", "remote", "https://mcp.example.com/mcp"}, config.TrustAsStored,
		mcpIO{in: strings.NewReader(""), out: &out, err: &out, inSandbox: true})
	if !strings.Contains(out.String(), "ABHED_SANDBOX") {
		t.Fatalf("without a terminal the refusal names something else: %s", out.String())
	}
}

// A managed configuration that sets the MCP servers forbids add and remove.
func TestMCPRefusedWhenManagedSetsServers(t *testing.T) {
	managedConfig(t, `{"mcp":{"servers":[]}}`)
	home, _ := os.UserHomeDir()
	write(t, filepath.Join(home, ".abhed", "config.json"), `{"mcp":{"servers":[{"name":"old","command":"/bin/old"}]}}`)
	if code, out := mcpRun(t, true, "y\n", "add", "docs", "/bin/docs"); code == 0 || !strings.Contains(out, "managed") {
		t.Fatalf("add under a managed list: %d %s", code, out)
	}
	if code, _ := mcpRun(t, true, "y\n", "remove", "old"); code == 0 {
		t.Fatal("remove under a managed list")
	}
	if len(userServers(userConfigDoc(t))) != 1 {
		t.Fatal("the user file changed")
	}
}

// Names, environment entries and headers are checked before anything is asked.
func TestMCPAddRefusesBadInput(t *testing.T) {
	managedConfig(t, "")
	for _, args := range [][]string{
		{"add", "bad name", "/bin/x"},
		{"add", "a\u202eb", "/bin/x"},
		{"add", "-env", "BAD NAME", "x", "/bin/x"},
		{"add", "-env", "=v", "x", "/bin/x"},
		{"add", "-header-env", "Authorization=TOKEN", "x", "/bin/x"},
		{"add", "-header-env", "Bad:Header=TOKEN", "x", "https://h.example/mcp"},
		{"add", "x", "https://h.example/mcp", "extra"},
		{"add", "x"},
	} {
		if code, _ := mcpRun(t, true, "y\n", args...); code == 0 {
			t.Fatalf("%q was accepted", args)
		}
	}
	if len(userServers(userConfigDoc(t))) != 0 {
		t.Fatal("a refused add wrote the file")
	}
}

// list shows where each server in effect came from.
func TestMCPList(t *testing.T) {
	managedConfig(t, "")
	home, _ := os.UserHomeDir()
	write(t, filepath.Join(home, ".abhed", "config.json"), `{"mcp":{"servers":[{"name":"mine","command":"/bin/mine","enabled":true}]}}`)
	code, out := mcpRun(t, false, "", "list")
	if code != 0 || !strings.Contains(out, "mine") || !strings.Contains(out, "user") || !strings.Contains(out, "enabled") {
		t.Fatalf("list: %d %s", code, out)
	}
}
