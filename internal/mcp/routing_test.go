package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/mcp/mcptest"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// stub is the server config that runs a stub MCP server named name.
func stub(t *testing.T, name string, cfg mcptest.Config) mcp.ServerConfig {
	t.Helper()
	cfg.Name = name
	cmd, args, err := mcptest.Command(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return mcp.ServerConfig{Name: name, Command: cmd, Args: args, Enabled: true}
}

// captureWarnings sends the gateway's warnings to a buffer for the test.
func captureWarnings(t *testing.T) *syncBuffer {
	t.Helper()
	var b syncBuffer
	old := mcp.WarnOut
	mcp.WarnOut = &b
	t.Cleanup(func() { mcp.WarnOut = old })
	return &b
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func connect(t *testing.T, cfgs ...mcp.ServerConfig) *mcp.Gateway {
	t.Helper()
	g := mcp.NewGateway()
	t.Cleanup(g.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if errs := g.Connect(ctx, cfgs); len(errs) > 0 {
		t.Fatal(errs)
	}
	return g
}

func toolNamed(g *mcp.Gateway, name string) tools.Tool {
	for _, t := range g.Tools() {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

// echoOf runs tool with args and reads the stub's echo.
func echoOf(t *testing.T, tool tools.Tool, args string) mcptest.Echo {
	t.Helper()
	res := tool.Run(context.Background(), nil, json.RawMessage(args))
	if res.IsError {
		t.Fatalf("%s: %s", tool.Name(), res.Content)
	}
	e, err := mcptest.ParseEcho(res.Content)
	if err != nil {
		t.Fatalf("%s answered %q: %v", tool.Name(), res.Content, err)
	}
	return e
}

func compact(t *testing.T, raw string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Three live servers offer a tool of the same name; each namespaced tool
// reaches its own server, with its arguments as the model sent them.
func TestSameToolNameOnThreeServersRoutesToEach(t *testing.T) {
	shared := []mcptest.Tool{{Name: "search", Description: "search"}, {Name: "get.item", Description: "get"}}
	g := connect(t,
		stub(t, "alpha", mcptest.Config{Tools: shared}),
		stub(t, "beta", mcptest.Config{Tools: shared}),
		stub(t, "gamma", mcptest.Config{Tools: shared}))
	if n := len(g.Tools()); n != 6 {
		t.Fatalf("%d tools, want 6", n)
	}
	for i, server := range []string{"alpha", "beta", "gamma"} {
		for _, remote := range []string{"search", "get.item"} {
			args := fmt.Sprintf(`{"q":"find é %d","n":[1,2,{"deep":true}],"s":"%s"}`, i, server)
			tool := toolNamed(g, "mcp__"+server+"__"+remote)
			if tool == nil {
				t.Fatalf("no tool mcp__%s__%s", server, remote)
			}
			e := echoOf(t, tool, args)
			if e.Server != server || e.Tool != remote {
				t.Fatalf("mcp__%s__%s reached server %q tool %q", server, remote, e.Server, e.Tool)
			}
			if got, want := compact(t, string(e.Args)), compact(t, args); got != want {
				t.Fatalf("arguments changed on the way: sent %s, server got %s", want, got)
			}
		}
	}
}

// A tool list in pages is read to its end, following nextCursor.
func TestToolsListFollowsNextCursor(t *testing.T) {
	g := connect(t, stub(t, "paged", mcptest.Config{Tools: mcptest.Tools("t", 25), PageSize: 10}))
	if n := len(g.Tools()); n != 25 {
		t.Fatalf("%d tools offered, want all 25 of the three pages", n)
	}
	if toolNamed(g, "mcp__paged__t24") == nil {
		t.Fatal("the last page's tool is missing")
	}
}

// A server that repeats its cursor is not read forever.
func TestToolsListStopsAtARepeatedCursor(t *testing.T) {
	warned := captureWarnings(t)
	g := connect(t, stub(t, "loop", mcptest.Config{Tools: mcptest.Tools("t", 9), PageSize: 3, RepeatCursor: true}))
	if n := len(g.Tools()); n != 6 {
		t.Fatalf("%d tools, want the first page and the one its repeated cursor named (6)", n)
	}
	if !strings.Contains(warned.String(), `mcp server "loop" repeated a cursor in tools/list`) {
		t.Fatalf("no warning names the server: %q", warned.String())
	}
}

// Past the page cap the rest of a list is left out, and the warning says so.
func TestToolsListPastThePageCapWarns(t *testing.T) {
	warned := captureWarnings(t)
	g := connect(t, stub(t, "huge", mcptest.Config{Tools: mcptest.Tools("t", mcp.MaxToolPages+5), PageSize: 1}))
	if n := len(g.Tools()); n != mcp.MaxToolPages {
		t.Fatalf("%d tools, want the first %d pages' worth", n, mcp.MaxToolPages)
	}
	if !strings.Contains(warned.String(), `mcp server "huge" lists more tools than are read`) {
		t.Fatalf("no warning: %q", warned.String())
	}
}

// Prompts past the 200 offered are left out with a warning, not silently.
func TestPromptsPastTheCapWarn(t *testing.T) {
	warned := captureWarnings(t)
	g := connect(t, stub(t, "talky", mcptest.Config{Tools: mcptest.Tools("t", 1), Prompts: 250, PageSize: 50}))
	if n := len(g.Prompts()); n != 200 {
		t.Fatalf("%d prompts, want 200", n)
	}
	if !strings.Contains(warned.String(), `mcp server "talky" lists more prompts than are read`) {
		t.Fatalf("no warning: %q", warned.String())
	}
}

// A server that writes a line past the bound ends its connection with an
// error that says why, rather than being read into memory without end.
func TestLongStdoutLineIsRefused(t *testing.T) {
	warned := captureWarnings(t)
	cmd, args, err := mcptest.Command(mcptest.Config{Name: "flood", Tools: mcptest.Tools("t", 1), LongLine: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tr, err := mcp.NewStdioTransport(ctx, cmd, args, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr.MaxLine = 16 << 10
	c := mcp.NewClient("flood", tr)
	defer c.Close()
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Call(ctx, "t0", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "longer than the limit") {
		t.Fatalf("call over a too-long line: %v", err)
	}
	if !strings.Contains(warned.String(), `mcp server "flood"`) {
		t.Fatalf("no warning names the server: %q", warned.String())
	}
}

// A server that never answers its handshake is reported once its startup
// deadline passes, its tools absent, and holds up no other server.
func TestHungServerHoldsUpNoOther(t *testing.T) {
	hung := stub(t, "hung", mcptest.Config{Hang: true, Tools: mcptest.Tools("t", 1)})
	hung.StartupTimeout = 1500 * time.Millisecond
	g := mcp.NewGateway()
	defer g.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	errs := g.Connect(ctx, []mcp.ServerConfig{hung,
		stub(t, "one", mcptest.Config{Tools: mcptest.Tools("a", 2)}),
		stub(t, "two", mcptest.Config{Tools: mcptest.Tools("b", 2)})})
	took := time.Since(start)
	if took > 10*time.Second {
		t.Fatalf("Connect took %s with one server hung", took)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), `mcp server "hung": did not start within 1.5s`) {
		t.Fatalf("errors: %v", errs)
	}
	if n := len(g.Tools()); n != 4 {
		t.Fatalf("%d tools, want the other servers' 4", n)
	}
	for _, st := range g.Servers() {
		if st.Name == "hung" && (st.Connected || st.Err == nil) {
			t.Fatalf("the hung server is listed as %+v", st)
		}
	}
}

// More hung servers than start slots: each gives up its slot after SlotHold,
// so the rest start then rather than a whole deadline later.
func TestManyHungServersDoNotQueueTheRest(t *testing.T) {
	const deadline = 1500 * time.Millisecond
	var cfgs []mcp.ServerConfig
	for i := range 2*mcp.DefaultConnectWorkers + 1 {
		c := stub(t, fmt.Sprintf("hung%d", i), mcptest.Config{Hang: true, Tools: mcptest.Tools("t", 1)})
		c.StartupTimeout = deadline
		cfgs = append(cfgs, c)
	}
	cfgs = append(cfgs, stub(t, "well", mcptest.Config{Tools: mcptest.Tools("a", 2)}))
	g := mcp.NewGateway()
	g.SlotHold = 100 * time.Millisecond
	defer g.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	errs := g.Connect(ctx, cfgs)
	// Queued behind whole deadlines, three groups of hung servers take three
	// deadlines and their shutdowns (about 7.5s); with the hold, about one.
	if took := time.Since(start); took >= 4*time.Second {
		t.Fatalf("Connect took %s with %d servers hung: they queued behind each other's deadlines", took, len(cfgs)-1)
	}
	if len(errs) != len(cfgs)-1 {
		t.Fatalf("%d errors, want one per hung server: %v", len(errs), errs)
	}
	if n := len(g.Tools()); n != 2 {
		t.Fatalf("%d tools, want the healthy server's 2", n)
	}
}

// Servers start together, a bounded number at a time, not one after another.
func TestServersStartTogether(t *testing.T) {
	var cfgs []mcp.ServerConfig
	for i := range 8 {
		cfgs = append(cfgs, stub(t, fmt.Sprintf("slow%d", i), mcptest.Config{Delay: 700 * time.Millisecond, Tools: mcptest.Tools("t", 1)}))
	}
	start := time.Now()
	g := connect(t, cfgs...)
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("eight servers of 0.7s each took %s: they started one after another", took)
	}
	if n := len(g.Tools()); n != 8 {
		t.Fatalf("%d tools", n)
	}
}
