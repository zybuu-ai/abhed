package toolset

import (
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/monitor"
)

func TestSupervisorFollowsTheConfiguration(t *testing.T) {
	cfg := config.Default()
	reg := tools.NewRegistry(tools.Bash{Isolation: tools.Isolation{Tier: "process"}})
	g := Supervisor(cfg, reg, false)
	if g == nil || g.Unattended || g.OnUnavailable != policy.Ask {
		t.Fatalf("default: %+v", g)
	}
	r, ok := g.Monitor.(monitor.Rules)
	if !ok || r.LookPath == nil || !r.LookPath("sh") || r.LookPath("no-such-program-anywhere") {
		t.Fatalf("the rules do not look on the host's PATH: %#v", g.Monitor)
	}
	if SupervisorLabel(cfg) != "rules" {
		t.Fatal(SupervisorLabel(cfg))
	}

	// Commands in a container: their programs are not the host's to look up.
	g = Supervisor(cfg, tools.NewRegistry(tools.Bash{Isolation: tools.Isolation{Tier: "container"}}), true)
	if r := g.Monitor.(monitor.Rules); r.LookPath != nil || !g.Unattended {
		t.Fatalf("container: %+v", g)
	}

	cfg.Monitor = config.MonitorConfig{Enabled: true, Judge: "model", Endpoint: "http://127.0.0.1:8861/v1", Model: "guard",
		APIKeyEnv: "ABHED_TEST_JUDGE_KEY", OnUnavailable: "deny", TimeoutMS: 500, MaxPerTurn: 4, Honeytokens: []string{"HONEYTOKEN000001"}}
	t.Setenv("ABHED_TEST_JUDGE_KEY", "k")
	g = Supervisor(cfg, reg, false)
	ch, ok := g.Monitor.(monitor.Chain)
	if !ok || len(ch) != 2 || g.OnUnavailable != policy.Deny || g.Timeout.Milliseconds() != 500 || g.MaxPerTurn != 4 || len(g.Honeytokens) != 1 {
		t.Fatalf("model: %+v", g)
	}
	if m := ch[1].(monitor.Model); m.APIKey != "k" || m.Model != "guard" {
		t.Fatalf("%+v", m)
	}
	if got := SupervisorLabel(cfg); got != "model (guard at http://127.0.0.1:8861, after the rules)" {
		t.Fatal(got)
	}

	cfg.Monitor.Enabled = false
	if Supervisor(cfg, reg, false) != nil || SupervisorLabel(cfg) != "off" {
		t.Fatal("off is not off")
	}
}
