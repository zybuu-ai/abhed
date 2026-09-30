//go:build unix

package clitest

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func toolNames(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range req.Tools {
		out = append(out, x.Function.Name)
	}
	return out
}

// Two hundred MCP tools do not fill the request: they are offered through
// tool_search, a found tool is offered from the next step, and calling it
// still goes through policy (allowed here by a rule) to the server.
func TestMCPDeferredLoading(t *testing.T) {
	t.Parallel()
	mcp := NewMCPServer(t, 200)
	h := piped(t, Opts{UserConfig: ConfigWithMCP("big", mcp.URL()),
		Args:   []string{"-p", "report on widget 137", "-allow", "mcp__big__tool_137", "-output-format", "stream-json"},
		Script: "tool tool_search {\"query\":\"widget 137\",\"limit\":1}\n\ntool mcp__big__tool_137 {\"widget\":\"137\"}\n\ntext \"done\""})
	if code := h.Wait(time.Second); code != 0 {
		t.Fatalf("exit %d:\n%s", code, h.Output())
	}
	reqs := h.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	first := toolNames(t, reqs[0].Body)
	for _, n := range first {
		if strings.HasPrefix(n, "mcp__") {
			t.Fatalf("an MCP tool was offered before a search: %s", n)
		}
	}
	if !slices.Contains(first, "tool_search") {
		t.Fatalf("no tool_search in %v", first)
	}
	if size := len(reqs[0].Body); size > 60<<10 {
		t.Errorf("the first request is %d bytes", size)
	}
	if !slices.Contains(toolNames(t, reqs[1].Body), "mcp__big__tool_137") {
		t.Fatal("the found tool was not offered next")
	}
	if !slices.Equal(mcp.Calls(), []string{"tool_137"}) {
		t.Fatalf("server calls %v", mcp.Calls())
	}
}

// Without an allow rule a deferred tool is refused in -p like any other.
func TestMCPDeferredToolIsPoliced(t *testing.T) {
	t.Parallel()
	mcp := NewMCPServer(t, 50)
	h := piped(t, Opts{UserConfig: ConfigWithMCP("big", mcp.URL()), Args: []string{"-p", "go"},
		Script: "tool mcp__big__tool_001 {}\n\ntext \"refused\""})
	h.Wait(time.Second)
	if len(mcp.Calls()) != 0 {
		t.Fatalf("a deferred tool ran without approval: %v", mcp.Calls())
	}
}

// The panels: /mcp with its tools and a restart, /tools, /agents, /skills,
// /release-notes, /bug and /doctor.
func TestPanelsOnPty(t *testing.T) {
	t.Parallel()
	mcp := NewMCPServer(t, 3)
	h := StartRun(t, Opts{UserConfig: ConfigWithMCP("small", mcp.URL()), Cols: 160, Rows: 50,
		Script: "tool get_time {\"zone\":\"UTC\"}"})
	h.WaitText("Type a task")
	h.Settle()
	h.Type("/mcp\r")
	h.WaitText("tool_000, tool_001, tool_002")
	h.Settle()
	h.Type("/mcp restart small\r")
	h.WaitText("reconnected small")
	h.Settle()
	h.Type("/tools\r")
	h.WaitText("mcp__small__tool_002")
	h.Settle()
	h.Type("/agents\r")
	h.WaitText("builtin")
	h.Settle()
	h.Type("/skills\r")
	h.WaitText("no skills are loaded")
	h.Settle()
	h.Type("/release-notes\r")
	h.WaitOutput("Unreleased")
	h.Settle()
	h.Type("/bug the screen flickers\r")
	h.WaitText("nothing has been sent")
	h.WaitOutput("https://github.com/zybuu-ai/abhed/issues/new?")
	if strings.Contains(Strip(h.Output()), h.Home()) {
		t.Fatal("the report holds the home directory")
	}
	h.Settle()
	h.Type("/doctor\r")
	h.WaitText("the model calls tools")
	h.Exit(0)
}
