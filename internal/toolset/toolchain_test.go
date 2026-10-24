package toolset

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/toolchain"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// An install asks with the language and major version as its scope, so one
// "always allow" covers that language and major; auto and preapproved add
// allow rules, and off or a managed hold on the permissions adds none.
func TestToolchainPolicy(t *testing.T) {
	pol := policy.New(policy.ModeDefault)
	args := json.RawMessage(`{"name":"go@1.22","resolved":{"version":"1.22.5"}}`)
	d := pol.Evaluate(toolchain.ToolName, true, args)
	if d.Decision != policy.Ask || d.Offer() != "toolchain(go@1.22)" {
		t.Fatalf("%+v", d)
	}
	for _, c := range []struct {
		cfg  config.Config
		want string
	}{
		{config.Config{Sandbox: config.SandboxConfig{Toolchains: "auto"}}, "toolchain"},
		{config.Config{Sandbox: config.SandboxConfig{ToolchainsPreapproved: []string{"go", "python@3.12.4", "node@20"}}}, "toolchain(go@*) toolchain(python@3.12) toolchain(node@20)"},
		{config.Config{Sandbox: config.SandboxConfig{Toolchains: "off", ToolchainsPreapproved: []string{"go"}}}, ""},
		{config.Config{Managed: true, ManagedKeys: []string{"permissions.allow"}, Sandbox: config.SandboxConfig{Toolchains: "auto"}}, ""},
		{config.Config{Managed: true, ManagedKeys: []string{"permissions.allow", "sandbox.toolchains"}, Sandbox: config.SandboxConfig{Toolchains: "auto"}}, "toolchain"},
	} {
		if got := strings.Join(ToolchainAllows(c.cfg), " "); got != c.want {
			t.Errorf("%+v: %q, want %q", c.cfg.Sandbox, got, c.want)
		}
	}
	pre := policy.New(policy.ModeDefault)
	_ = pre.AddAllow(ToolchainAllows(config.Config{Sandbox: config.SandboxConfig{ToolchainsPreapproved: []string{"go"}}})...)
	if d := pre.Evaluate(toolchain.ToolName, true, args); d.Decision != policy.Allow {
		t.Fatalf("preapproved go: %+v", d)
	}
	if d := pre.Evaluate(toolchain.ToolName, true, json.RawMessage(`{"name":"node@20"}`)); d.Decision != policy.Ask {
		t.Fatalf("node, not preapproved: %+v", d)
	}
}

// Bash's commands start with the active toolchains on PATH, inside the
// sandbox; a git command the host runs for the sandbox is passed on as is.
func TestBashGetsTheToolchains(t *testing.T) {
	root := t.TempDir()
	tc := &toolchain.Session{Cache: toolchain.Cache{Root: root}, Platform: toolchain.Host}
	var lines []string
	b := withToolchains(tools.Bash{Sandbox: func(ctx context.Context, cwd, line string) *exec.Cmd {
		lines = append(lines, line)
		return exec.Command("true")
	}}, tc)
	if b.NotFound == nil {
		t.Fatal("no NotFound")
	}
	b.Sandbox(context.Background(), t.TempDir(), "go version")
	if lines[0] != "go version" {
		t.Fatalf("with nothing installed: %q", lines[0])
	}
	off := withToolchains(tools.Bash{Sandbox: b.Sandbox}, &toolchain.Session{Off: true})
	if off.NotFound == nil || !strings.Contains(off.NotFound("", "go"), "turned off") {
		t.Fatal("off says so")
	}
}
