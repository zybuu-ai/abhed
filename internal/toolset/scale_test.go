//go:build scale

package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/mcp/mcptest"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// The integration scale check, trimmed to run in a minute: 50 stub MCP
// servers of 20 tools, 300 skills and 100 agent types in one session. It
// checks that calls on a sample reach the right server unchanged, that a
// long session's requests stay within the provider's tool limit, and that
// the listings stay within their budgets. Run it with make test-scale.
const (
	scaleServers = 50
	scaleTools   = 20
	scaleSkills  = 300
	scaleAgents  = 100
)

func TestScaleIntegrations(t *testing.T) {
	servers := map[string][]mcptest.Tool{}
	for i := range scaleServers {
		ts := mcptest.Tools("op", scaleTools)
		// Some names a provider would refuse: dotted, and long.
		ts[0].Name = fmt.Sprintf("query.records.v%d", i)
		ts[1].Name = fmt.Sprintf("summarise_the_changes_made_to_the_records_of_this_server_since_a_given_time_%d", i)
		servers[fmt.Sprintf("srv%02d", i)] = ts
	}
	start := time.Now()
	g := stubServers(t, servers)
	startup := time.Since(start)

	skillRoot := t.TempDir()
	for i := range scaleSkills {
		dir := filepath.Join(skillRoot, fmt.Sprintf("team-runbook-%03d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		md := fmt.Sprintf("---\nname: team-runbook-%03d\ndescription: Use when the request is about service %03d: its deploys, its alerts, its owners and the steps the team agreed for it.\n---\n\nSteps for service %03d.\n", i, i, i)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sk, errs := skills.Load([]string{skillRoot})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	var defs []*agent.Definition
	for i := range scaleAgents {
		defs = append(defs, &agent.Definition{Name: fmt.Sprintf("service-specialist-%03d", i),
			Description: fmt.Sprintf("Use for work on service %03d: reading its code, its dashboards and its incident history.", i)})
	}
	agents := agent.WithDefinitions(defs...)

	reg := withMCP(builtins(), g)
	reg.Add(SkillTool(sk))
	reg.Add(agent.Task{Agents: agents})
	reg.Add(agent.Tasks{Agents: agents})

	// A long session: fifteen searches of ten tools each.
	search, _ := reg.Get("tool_search")
	for i := range 15 {
		res := search.Run(context.Background(), nil, json.RawMessage(fmt.Sprintf(`{"query":"srv%02d op","limit":10}`, i*3)))
		if res.IsError {
			t.Fatal(res.Content)
		}
	}

	// Routing on a sample, through the registry as the loop calls tools.
	sampled := 0
	for i := 0; i < scaleServers; i += 7 {
		server := fmt.Sprintf("srv%02d", i)
		for _, remote := range []string{"op5", servers[server][0].Name, servers[server][1].Name} {
			name := "mcp__" + server + "__" + remote
			e := run(t, reg, name, fmt.Sprintf(`{"q":"sample %d"}`, i))
			if e.Server != server || e.Tool != remote || string(e.Args) != fmt.Sprintf(`{"q":"sample %d"}`, i) {
				t.Fatalf("%s reached %+v", name, e)
			}
			sampled++
		}
	}

	// Through the loop and a provider adapter: a dotted and a long tool,
	// loaded, called by their wire names.
	dotted, long := "mcp__srv07__query.records.v7", "mcp__srv07__"+servers["srv07"][1].Name
	script := &openAIScript{turns: [][]string{toolCallFrame(
		[3]string{"c1", model.WireName(dotted), `{"q":"d"}`},
		[3]string{"c2", model.WireName(long), `{"q":"l"}`},
	)}}
	srv := httptest.NewServer(script)
	defer srv.Close()
	adapter := model.NewOpenAICompatible(srv.URL, "", "m", model.Profile{ContextWindow: 200000})
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := agent.NewLoop(adapter, reg, policy.New(policy.ModeDefault), agent.AutoApprove{Yes: true}, sess,
		agent.NewRecorder(agent.NewMemStore(), "s", ""), agent.DefaultConfig())
	l.Config.SystemPrompt = sk.Listing()
	if _, err := l.Run(context.Background(), "use them"); err != nil {
		t.Fatal(err)
	}
	results := toolResults(script.bodies[1])
	for id, want := range map[string]string{"c1": "query.records.v7", "c2": servers["srv07"][1].Name} {
		if !strings.Contains(results[id], `"server":"srv07"`) || !strings.Contains(results[id], `"tool":"`+want+`"`) {
			t.Fatalf("%s: %q", id, results[id])
		}
	}

	// What the requests offered.
	maxTools, toolBytes := 0, 0
	for _, body := range script.bodies {
		names := requestTools(body)
		maxTools = max(maxTools, len(names))
		for _, n := range names {
			if !model.ValidWireName(n) {
				t.Fatalf("offered %q, which a provider refuses", n)
			}
		}
		raw, _ := json.Marshal(body["tools"])
		toolBytes = max(toolBytes, len(raw))
	}
	listing := sk.Listing()
	skillTool, _ := reg.Get("skill")
	task, _ := reg.Get("task")
	tasks, _ := reg.Get("tasks")
	loaded := 0
	for _, d := range reg.Definitions() {
		if strings.HasPrefix(d.Name, "mcp__") {
			loaded++
		}
	}
	t.Logf("servers %d, MCP tools %d, startup %s", len(g.Servers()), len(g.Tools()), startup.Round(time.Millisecond))
	t.Logf("sampled calls routed: %d; loop calls by wire name: 2", sampled)
	t.Logf("tools offered per request: at most %d (limit %d); MCP tools loaded after 150 searched: %d", maxTools, model.DefaultMaxTools, loaded)
	t.Logf("tool definitions: %d bytes, about %d tokens", toolBytes, toolBytes*10/36)
	t.Logf("skill listing %d bytes, skill tool %d bytes; task tool %d bytes + schema %d, tasks schema %d",
		len(listing), len(skillTool.Description()), len(task.Description()), len(task.Schema()), len(tasks.Schema()))
	fixed := len(listing) + toolBytes
	t.Logf("listing and tools before the first message: %d bytes, about %d tokens", fixed, fixed*10/36)

	if len(g.Tools()) != scaleServers*scaleTools {
		t.Fatalf("%d MCP tools, want %d", len(g.Tools()), scaleServers*scaleTools)
	}
	if maxTools > model.DefaultMaxTools {
		t.Fatalf("a request offered %d tools", maxTools)
	}
	if loaded > MaxLoaded {
		t.Fatalf("%d tools loaded", loaded)
	}
	if len(listing) > skills.ListingBudget || len(skillTool.Description()) > skills.ListingBudget+512 {
		t.Fatalf("skill listing %d, skill tool %d bytes", len(listing), len(skillTool.Description()))
	}
	if len(task.Description()) > agent.ListingBudget+1024 || len(task.Schema()) > agent.ListingBudget+2048 {
		t.Fatalf("task tool %d, schema %d bytes", len(task.Description()), len(task.Schema()))
	}
}
