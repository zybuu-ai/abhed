package toolset

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	if err := admit(mcp.ServerConfig{Name: "x", URL: "https://a.evil.test/mcp"}); err == nil {
		t.Fatal("a denied host was admitted")
	}
	if err := admit(mcp.ServerConfig{Name: "y", URL: "https://good.example/mcp"}); err != nil {
		t.Fatalf("an unlisted host under a deny list alone: %v", err)
	}
}
