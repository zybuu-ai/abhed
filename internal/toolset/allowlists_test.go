package toolset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/monitor"
)

func allowCfg() config.Config {
	cfg := config.Default()
	cfg.Model.Providers["mine"] = config.ProviderConfig{Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", Model: "ok-1", ContextWindow: 8192}
	cfg.Model.Providers["theirs"] = config.ProviderConfig{Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", Model: "no-1", ContextWindow: 8192}
	cfg.Model.Default = "mine"
	cfg.Model.Allowed = []string{"openai-compatible/ok-*"}
	return cfg
}

// A subagent's model is held to the allowed models, with the reason.
func TestSubagentModelAllowed(t *testing.T) {
	cfg := allowCfg()
	if _, err := ModelResolver(cfg)("mine"); err != nil {
		t.Fatalf("admitted: %v", err)
	}
	_, err := ModelResolver(cfg)("theirs")
	var me *config.ManagedError
	if !errors.As(err, &me) || me.Key != "model.allowed" {
		t.Fatalf("refused: %v", err)
	}
	if got := OfferedModels(cfg); len(got) != 1 || got[0] != "mine" {
		t.Fatalf("offered: %v", got)
	}
}

// A model judge the allowed models refuse is never asked: its calls get what
// an unavailable judge gives.
func TestSupervisorJudgeModelAllowed(t *testing.T) {
	cfg := allowCfg()
	cfg.Monitor = config.MonitorConfig{Enabled: true, Judge: "model", Endpoint: "http://127.0.0.1:8861/v1", Model: "guard"}
	g := Supervisor(cfg, tools.NewRegistry(), false)
	ch := g.Monitor.(monitor.Chain)
	if _, err := ch[1].Judge(context.Background(), monitor.Case{}); err == nil || !strings.Contains(err.Error(), "model.allowed") {
		t.Fatalf("judge: %T %v", ch[1], err)
	}
	cfg.Model.Allowed = append(cfg.Model.Allowed, "openai-compatible/guard")
	if _, ok := Supervisor(cfg, tools.NewRegistry(), false).Monitor.(monitor.Chain)[1].(monitor.Model); !ok {
		t.Fatal("an admitted judge model is not asked")
	}
}

func TestMCPAdmitFollowsTheLists(t *testing.T) {
	cfg := config.Default()
	if MCPAdmit(cfg) != nil {
		t.Fatal("no lists, no check")
	}
	cfg.MCP.Denied = []config.MCPMatch{{Host: "*.evil.test"}}
	admit := MCPAdmit(cfg)
	if err := admit(mcp.ServerConfig{Name: "x", URL: "https://a.evil.test/mcp"}, nil); err == nil {
		t.Fatal("a denied host was admitted")
	}
	if err := admit(mcp.ServerConfig{Name: "y", URL: "https://good.example/mcp"}, nil); err != nil {
		t.Fatalf("an unlisted host under a deny list alone: %v", err)
	}
	if err := admit(mcp.ServerConfig{Name: "z", URL: "https://a.ｅｖｉｌ.test/mcp"}, nil); err == nil {
		t.Fatal("a fullwidth form of a denied host was admitted")
	}
	if cfg.MCPListsMatchCommands() {
		t.Fatal("no command entry, yet servers would be pinned to a path")
	}
	cfg.MCP.Allowed = []config.MCPMatch{{Command: "/opt/mcp/bin/*"}}
	if !cfg.MCPListsMatchCommands() {
		t.Fatal("a command entry does not pin the program started")
	}
	if err := MCPAdmit(cfg)(mcp.ServerConfig{Name: "s", Command: "s"}, []string{"/opt/mcp/bin/s"}); err != nil {
		t.Fatalf("the program the gateway resolved: %v", err)
	}
}

// A program named docker at a path the user chooses, its run naming an image
// by the digest an allow entry names, is not admitted by that digest alone:
// the digest pins an image, nothing about the program, so it never runs. An
// entry that names the engine's path as well admits it.
func TestImagePinNeedsTheEngineCommand(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	for _, c := range []struct {
		name    string
		allowed func(dir string) []config.MCPMatch
		runs    bool
	}{
		{"digest only", func(string) []config.MCPMatch { return []config.MCPMatch{{Name: "github", Digest: digest}} }, false},
		{"digest only, with a command entry elsewhere", func(string) []config.MCPMatch {
			return []config.MCPMatch{{Name: "github", Digest: digest}, {Command: "/opt/mcp/bin/*"}}
		}, false},
		{"digest with the engine's path", func(dir string) []config.MCPMatch {
			real, _ := filepath.EvalSymlinks(dir)
			return []config.MCPMatch{{Name: "github", Command: filepath.Join(real, "docker"), Digest: digest}}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, marks := t.TempDir(), t.TempDir()
			mark := filepath.Join(marks, "ran")
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\necho ran > "+mark+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.MCP.Allowed = c.allowed(dir)
			g := mcp.NewGateway()
			defer g.Close()
			g.Admit, g.PinProgram = MCPAdmit(cfg), cfg.MCPListsMatchCommands()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs := g.Connect(ctx, []mcp.ServerConfig{{Name: "github", Command: filepath.Join(dir, "docker"),
				Args: []string{"run", "a@" + digest}, Digest: digest, Enabled: true}})
			_, err := os.Stat(mark)
			if ran := err == nil; ran != c.runs {
				t.Fatalf("ran %v, want %v (errors %v)", ran, c.runs, errs)
			}
			if !c.runs && (len(errs) != 1 || !errors.Is(errs[0], mcp.ErrNotAdmitted)) {
				t.Fatalf("want one refusal by the lists: %v", errs)
			}
		})
	}
}
