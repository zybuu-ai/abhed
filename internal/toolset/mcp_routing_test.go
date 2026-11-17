package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/mcp/mcptest"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/plugin"
)

// stubServers connects one stub MCP server per name, each with tools.
func stubServers(t testing.TB, servers map[string][]mcptest.Tool) *mcp.Gateway {
	t.Helper()
	var cfgs []mcp.ServerConfig
	for name, ts := range servers {
		cmd, args, err := mcptest.Command(mcptest.Config{Name: name, Tools: ts})
		if err != nil {
			t.Fatal(err)
		}
		cfgs = append(cfgs, mcp.ServerConfig{Name: name, Command: cmd, Args: args, Enabled: true})
	}
	g := mcp.NewGateway()
	t.Cleanup(g.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if errs := g.Connect(ctx, cfgs); len(errs) > 0 {
		t.Fatal(errs)
	}
	return g
}

// builtins stands for the built-in tools a session offers.
func builtins() *tools.Registry {
	return tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Edit{}, tools.Glob{}, tools.Grep{}, tools.Bash{},
		agent.ShellOutput{}, agent.ShellKill{}, agent.TodoTool{})
}

// withMCP is reg with the gateway's tools, deferred as Build defers them.
func withMCP(reg *tools.Registry, g *mcp.Gateway) *tools.Registry {
	for _, t := range g.Tools() {
		reg.Add(t)
	}
	DeferMCP(reg, DeferThreshold)
	return reg
}

func run(t testing.TB, reg *tools.Registry, name, args string) mcptest.Echo {
	t.Helper()
	tool, ok := reg.Get(name)
	if !ok {
		t.Fatalf("no tool %s", name)
	}
	res := tool.Run(context.Background(), nil, json.RawMessage(args))
	if res.IsError {
		t.Fatalf("%s: %s", name, res.Content)
	}
	e, err := mcptest.ParseEcho(res.Content)
	if err != nil {
		t.Fatalf("%s: %q", name, res.Content)
	}
	return e
}

func offered(reg *tools.Registry) map[string]bool {
	out := map[string]bool{}
	for _, d := range reg.Definitions() {
		out[d.Name] = true
	}
	return out
}

// A deferred tool, once tool_search loads it, is offered and reaches its own
// server with its arguments unchanged.
func TestDeferredToolOnceLoadedRoutes(t *testing.T) {
	g := stubServers(t, map[string][]mcptest.Tool{
		"alpha": mcptest.Tools("t", 20), "beta": mcptest.Tools("t", 20), "gamma": mcptest.Tools("t", 20)})
	reg := withMCP(builtins(), g)
	if offered(reg)["mcp__beta__t7"] {
		t.Fatal("60 tools were not deferred")
	}
	search, _ := reg.Get("tool_search")
	if res := search.Run(context.Background(), nil, json.RawMessage(`{"query":"mcp__beta__t7"}`)); res.IsError || !strings.Contains(res.Content, "mcp__beta__t7") {
		t.Fatalf("search: %s", res.Content)
	}
	if !offered(reg)["mcp__beta__t7"] {
		t.Fatal("the loaded tool is not offered")
	}
	e := run(t, reg, "mcp__beta__t7", `{"q":"x é"}`)
	if e.Server != "beta" || e.Tool != "t7" || string(e.Args) != `{"q":"x é"}` {
		t.Fatalf("reached %+v", e)
	}
}

// tool_search keeps at most MaxLoaded tools loaded, unloading the least
// recently used, and never takes a built-in out of the offer.
func TestToolSearchUnloadsLeastRecentlyUsed(t *testing.T) {
	servers := map[string][]mcptest.Tool{}
	for i := range 6 {
		servers[fmt.Sprintf("s%d", i)] = mcptest.Tools("t", 25)
	}
	reg := withMCP(builtins(), stubServers(t, servers))
	search, _ := reg.Get("tool_search")
	load := func(name string) {
		t.Helper()
		if res := search.Run(context.Background(), nil, json.RawMessage(`{"query":"`+name+`","limit":1}`)); res.IsError {
			t.Fatal(res.Content)
		}
	}
	load("mcp__s0__t0")
	for s := range 6 {
		for i := range 25 {
			if s == 0 && i == 0 {
				continue
			}
			load(fmt.Sprintf("mcp__s%d__t%d", s, i))
			if s == 0 && i == 10 {
				// A call keeps the first tool in use.
				run(t, reg, "mcp__s0__t0", `{}`)
			}
		}
	}
	got := offered(reg)
	loaded := 0
	for n := range got {
		if strings.HasPrefix(n, "mcp__") {
			loaded++
		}
	}
	if loaded != MaxLoaded {
		t.Fatalf("%d MCP tools loaded after 150 searches, want %d", loaded, MaxLoaded)
	}
	for _, n := range builtins().Names() {
		if !got[n] {
			t.Fatalf("built-in %s is no longer offered", n)
		}
	}
	if !got["mcp__s5__t24"] || got["mcp__s0__t1"] {
		t.Fatal("the most recent tool was unloaded, or the least recent kept")
	}
	if got["mcp__s0__t0"] {
		// Called at the eleventh load, it is older than the last hundred.
		t.Fatal("a tool not used for 139 loads is still loaded")
	}
}

// openAIScript is a stub OpenAI-compatible endpoint: each request gets the
// next turn's frames, and every request body is kept.
type openAIScript struct {
	mu     sync.Mutex
	turns  [][]string
	bodies []map[string]any
}

func (s *openAIScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	var frames []string
	if len(s.turns) > 0 {
		frames, s.turns = s.turns[0], s.turns[1:]
	} else {
		frames = []string{`{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, f := range frames {
		fmt.Fprintf(w, "data: %s\n\n", f)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func toolCallFrame(calls ...[3]string) []string {
	var parts []string
	for i, c := range calls {
		args, _ := json.Marshal(c[2])
		parts = append(parts, fmt.Sprintf(`{"index":%d,"id":%q,"function":{"name":%q,"arguments":%s}}`, i, c[0], c[1], args))
	}
	return []string{`{"choices":[{"delta":{"tool_calls":[` + strings.Join(parts, ",") + `]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`}
}

// requestTools are the tool names a request body offered.
func requestTools(body map[string]any) []string {
	var out []string
	ts, _ := body["tools"].([]any)
	for _, t := range ts {
		fn, _ := t.(map[string]any)["function"].(map[string]any)
		name, _ := fn["name"].(string)
		out = append(out, name)
	}
	return out
}

// toolResults are the tool messages a request body carries, by call id.
func toolResults(body map[string]any) map[string]string {
	out := map[string]string{}
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" {
			id, _ := mm["tool_call_id"].(string)
			text, _ := mm["content"].(string)
			out[id] = text
		}
	}
	return out
}

// A scripted model, through the loop and a real provider adapter, calls MCP
// tools on live stub servers: two servers' tools of one name, and a dotted,
// long one that crosses to the provider under its wire name. Each reaches its
// own server with the arguments the model wrote, and the request offered
// stays within the provider's limit with every name one it accepts.
func TestScriptedModelThroughLoopReachesMCP(t *testing.T) {
	long := "fetch.page.by.its.canonical.address.and.return.the.rendered.text"
	g := stubServers(t, map[string][]mcptest.Tool{
		"alpha": {{Name: "search", Description: "alpha search"}},
		"beta":  {{Name: "search", Description: "beta search"}},
		"docs":  {{Name: long, Description: "fetch a page"}},
	})
	reg := withMCP(builtins(), g)
	realLong := "mcp__docs__" + long
	wire := model.WireName(realLong)
	if wire == realLong {
		t.Fatal("the test needs a name the provider refuses")
	}
	script := &openAIScript{turns: [][]string{toolCallFrame(
		[3]string{"c1", "mcp__alpha__search", `{"q":"from alpha"}`},
		[3]string{"c2", "mcp__beta__search", `{"q":"from beta"}`},
		[3]string{"c3", wire, `{"q":"https://example.invalid/a b \"é\""}`},
	)}}
	srv := httptest.NewServer(script)
	defer srv.Close()
	adapter := model.NewOpenAICompatible(srv.URL, "", "m", model.Profile{ContextWindow: 100000})

	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := agent.NewLoop(adapter, reg, policy.New(policy.ModeDefault), agent.AutoApprove{Yes: true}, sess,
		agent.NewRecorder(agent.NewMemStore(), "s", ""), agent.DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := l.Run(ctx, "use the tools"); err != nil {
		t.Fatal(err)
	}
	if len(script.bodies) < 2 {
		t.Fatalf("%d requests", len(script.bodies))
	}
	for i, body := range script.bodies {
		names := requestTools(body)
		if len(names) > model.DefaultMaxTools {
			t.Fatalf("request %d offered %d tools", i, len(names))
		}
		for _, n := range names {
			if !model.ValidWireName(n) {
				t.Fatalf("request %d offered %q, which a provider refuses", i, n)
			}
		}
	}
	results := toolResults(script.bodies[1])
	want := map[string]mcptest.Echo{
		"c1": {Server: "alpha", Tool: "search", Args: json.RawMessage(`{"q":"from alpha"}`)},
		"c2": {Server: "beta", Tool: "search", Args: json.RawMessage(`{"q":"from beta"}`)},
		"c3": {Server: "docs", Tool: long, Args: json.RawMessage(`{"q":"https://example.invalid/a b \"é\""}`)},
	}
	for id, w := range want {
		text := results[id]
		start := strings.Index(text, `{"args"`)
		if start < 0 {
			t.Fatalf("%s: result %q", id, text)
		}
		e, err := mcptest.ParseEcho(text[start : strings.LastIndex(text, "}")+1])
		if err != nil {
			t.Fatalf("%s: %q: %v", id, text, err)
		}
		// The same values: the loop may write the object's keys in another order.
		var a, b any
		_ = json.Unmarshal(e.Args, &a)
		_ = json.Unmarshal(w.Args, &b)
		if e.Server != w.Server || e.Tool != w.Tool || !reflect.DeepEqual(a, b) {
			t.Fatalf("%s reached %s/%s with %s, want %s/%s with %s", id, e.Server, e.Tool, e.Args, w.Server, w.Tool, w.Args)
		}
	}
}

// Past the provider's limit the loop unloads searched-for tools, least
// recently used first, so the request stays within it; built-ins stay.
func TestLoopKeepsTheRequestWithinTheProviderLimit(t *testing.T) {
	servers := map[string][]mcptest.Tool{}
	for i := range 6 {
		servers[fmt.Sprintf("s%d", i)] = mcptest.Tools("t", 25)
	}
	reg := withMCP(builtins(), stubServers(t, servers))
	search, _ := reg.Get("tool_search")
	for s := range 6 {
		for i := range 25 {
			search.Run(context.Background(), nil, json.RawMessage(fmt.Sprintf(`{"query":"mcp__s%d__t%d","limit":1}`, s, i)))
		}
	}
	script := &openAIScript{}
	srv := httptest.NewServer(script)
	defer srv.Close()
	// A provider that takes 64 tools: the hundred loaded must give way.
	adapter := model.NewOpenAICompatible(srv.URL, "", "m", model.Profile{ContextWindow: 100000, MaxTools: 64})
	sess, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := agent.NewLoop(adapter, reg, policy.New(policy.ModeDefault), agent.AutoApprove{Yes: true}, sess,
		agent.NewRecorder(agent.NewMemStore(), "s", ""), agent.DefaultConfig())
	if _, err := l.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	names := requestTools(script.bodies[0])
	if len(names) > 64 {
		t.Fatalf("offered %d tools to a provider that takes 64", len(names))
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, n := range builtins().Names() {
		if !got[n] {
			t.Fatalf("built-in %s was dropped", n)
		}
	}
	if !got["mcp__s5__t24"] {
		t.Fatal("the most recently loaded tool was unloaded")
	}
}

// Load state is a session's own: each session's registry is a clone, and a
// search or an unload in one leaves another's offer as it was.
func TestLoadStateIsPerSession(t *testing.T) {
	servers := map[string][]mcptest.Tool{}
	for i := range 3 {
		servers[fmt.Sprintf("s%d", i)] = mcptest.Tools("t", 20)
	}
	base := withMCP(builtins(), stubServers(t, servers))
	a, b := base.Clone(), base.Clone()
	search := func(r *tools.Registry, q string) {
		t.Helper()
		s, _ := r.Get("tool_search")
		if res := s.Run(context.Background(), nil, json.RawMessage(`{"query":"`+q+`","limit":1}`)); res.IsError {
			t.Fatal(res.Content)
		}
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(2)
		go func() { defer wg.Done(); search(a, fmt.Sprintf("mcp__s0__t%d", i)) }()
		go func() { defer wg.Done(); search(b, fmt.Sprintf("mcp__s1__t%d", i)) }()
	}
	wg.Wait()
	for name, r := range map[string]*tools.Registry{"a": a, "b": b} {
		for n := range offered(r) {
			if strings.HasPrefix(n, "mcp__") && !strings.HasPrefix(n, map[string]string{"a": "mcp__s0__", "b": "mcp__s1__"}[name]) {
				t.Fatalf("session %s offers %s, which another session loaded", name, n)
			}
		}
	}
	if offered(base)["mcp__s0__t0"] {
		t.Fatal("a session's search loaded the tool in the shared registry")
	}
	before := len(offered(b))
	if n := a.UnloadLeastUsed(100); n != 20 {
		t.Fatalf("unloaded %d in a", n)
	}
	if len(offered(b)) != before {
		t.Fatal("unloading in one session unloaded tools in another")
	}
	// A subagent's subset is its own too.
	sub := a.Without([]string{"bash"})
	search(sub, "mcp__s2__t3")
	if offered(a)["mcp__s2__t3"] {
		t.Fatal("a subagent's search loaded the tool in its parent")
	}
}

// A tool whose name, or whose wire name, another tool already has is not
// registered, with a warning, and the first keeps it.
func TestAdderRefusesTakenNamesAndWireNames(t *testing.T) {
	reg := tools.NewRegistry(tools.Read{}, fakeMCP{"mcp__x__a.b", "dotted"})
	var warned []string
	add := adder(reg, func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) })
	if add(fakeMCP{"read", "an extension's"}) || add(fakeMCP{model.WireName("mcp__x__a.b"), "its twin"}) {
		t.Fatal("a taken name was registered")
	}
	if !add(fakeMCP{"mcp__x__c", "new"}) || len(warned) != 2 || !strings.Contains(warned[1], "would reach the model provider as") {
		t.Fatalf("warnings %q", warned)
	}
}

// A plugin's skill is capped at 300 characters like any other, with a
// warning naming the plugin.
func TestPluginSkillDescriptionIsCapped(t *testing.T) {
	reg := skills.NewRegistry()
	var warned []string
	md := "---\nname: wordy\ndescription: " + strings.Repeat("a", 900) + "\n---\n\nsteps\n"
	addPluginSkills(reg, []plugin.Loaded{{Name: "acme", Root: t.TempDir(),
		Skills: []plugin.LoadedSkill{{Name: "wordy", Dir: t.TempDir(), SkillMD: []byte(md)}}}},
		func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) })
	s, ok := reg.Get("wordy")
	if !ok || len([]rune(s.Description)) != skills.MaxDescription {
		t.Fatalf("loaded %v, description %d characters", ok, len(s.Description))
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "the plugin acme's skill wordy") || !strings.Contains(warned[0], "longer than 300") {
		t.Fatalf("warnings %q", warned)
	}
}

// A server added at run time goes through the same checks and deferral as at
// startup, and a removed tool leaves tool_search with it.
func TestAddMCPToolsDefersAndChecks(t *testing.T) {
	reg := withMCP(builtins(), stubServers(t, map[string][]mcptest.Tool{"a": mcptest.Tools("t", 50)}))
	g := stubServers(t, map[string][]mcptest.Tool{"b": mcptest.Tools("u", 20)})
	ts := g.Tools()
	ts = append(ts, fakeMCP{"mcp__z__q.r", ""}, fakeMCP{model.WireName("mcp__z__q.r"), ""}, fakeMCP{"read", ""})
	added, refused := AddMCPTools(reg, ts, func(string, ...any) {})
	if len(added) != 21 || len(refused) != 2 || refused[1] != "read" {
		t.Fatalf("added %d, refused %v", len(added), refused)
	}
	if offered(reg)["mcp__b__u3"] {
		t.Fatal("a server added at run time is offered in full past the threshold")
	}
	search, _ := reg.Get("tool_search")
	if res := search.Run(context.Background(), nil, json.RawMessage(`{"query":"mcp__b__u3","limit":1}`)); !strings.Contains(res.Content, "mcp__b__u3") {
		t.Fatalf("search: %s", res.Content)
	}
	reg.Remove("mcp__a__t7")
	AddMCPTools(reg, nil, func(string, ...any) {})
	search, _ = reg.Get("tool_search")
	if strings.Contains(search.Description(), "t7,") || strings.Contains(search.Run(context.Background(), nil, json.RawMessage(`{"query":"mcp__a__t7"}`)).Content, "mcp__a__t7\n") {
		t.Fatal("a removed tool is still searched")
	}
	if _, isDeferred := mustGet(t, reg, "mcp__a__t1").(*deferredTool); !isDeferred {
		t.Fatal("re-deferring wrapped a deferred tool again")
	}
}

func mustGet(t *testing.T, reg *tools.Registry, name string) tools.Tool {
	t.Helper()
	tl, ok := reg.Get(name)
	if !ok {
		t.Fatalf("no %s", name)
	}
	return tl
}
