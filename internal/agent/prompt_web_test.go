package agent

import (
	"strings"
	"testing"
)

// The prompt names a web tool only when the session has it: naming a missing
// one sent the model to curl in a sandbox with no network.
func TestPromptNamesOnlyTheWebToolsRegistered(t *testing.T) {
	ws := t.TempDir()
	for _, tc := range []struct {
		name        string
		tools       []string
		search      bool
		fetch       bool
		unavailable bool
	}{
		{"none", []string{"read", "bash"}, false, false, true},
		{"unknown", nil, false, false, true},
		{"search", []string{"read", "web_search"}, true, false, false},
		{"fetch", []string{"read", "web_fetch"}, false, true, false},
		{"both", []string{"web_fetch", "web_search"}, true, true, false},
	} {
		p := BuildSystemPrompt(BuildOptions{Profile: "main", Workspace: ws, Tools: tc.tools})
		if got := strings.Contains(p, "web_search"); got != tc.search {
			t.Errorf("%s: names web_search %v, want %v", tc.name, got, tc.search)
		}
		if got := strings.Contains(p, "web_fetch"); got != tc.fetch {
			t.Errorf("%s: names web_fetch %v, want %v", tc.name, got, tc.fetch)
		}
		if got := strings.Contains(p, "has no web tool"); got != tc.unavailable {
			t.Errorf("%s: says there is no web tool %v, want %v", tc.name, got, tc.unavailable)
		}
		if strings.Contains(p, "{{web}}") {
			t.Errorf("%s: placeholder left in the prompt", tc.name)
		}
	}
}

// With MCP tools behind tool_search, the connected-services line comes before
// the web lines, and the web, no-web and current-fact lines defer to it, so a
// live-data question reaches a listed tool (R6: weather_now). Without it, the
// prompt does not mention it.
func TestPromptPutsToolSearchBeforeTheWeb(t *testing.T) {
	ws := t.TempDir()
	for _, tc := range []struct {
		name  string
		tools []string
		web   string // the line the connected-services line must precede
	}{
		{"web on", []string{"read", "web_search", "web_fetch", "tool_search"}, "- Web search: for what tool_search finds no tool for"},
		{"search only", []string{"tool_search", "web_search"}, "- Web search: for what tool_search finds no tool for"},
		{"fetch only", []string{"tool_search", "web_fetch"}, "- Web pages: web_fetch"},
		{"web off", []string{"read", "tool_search"}, "- The web: this session has no web tool. For current fact that tool_search\n  finds no tool for"},
	} {
		p := BuildSystemPrompt(BuildOptions{Profile: "main", Workspace: ws, Tools: tc.tools})
		mcp := strings.Index(p, "- Connected services: MCP tools not offered directly are listed by server in\n  tool_search's description")
		web := strings.Index(p, tc.web)
		switch {
		case mcp < 0:
			t.Errorf("%s: deferred tools, but no connected-services line", tc.name)
		case web < 0:
			t.Errorf("%s: no deferring web line %q", tc.name, tc.web)
		case mcp > web:
			t.Errorf("%s: the web line comes before the connected-services line", tc.name)
		}
		for _, want := range []string{
			"call tool_search with a keyword from the request (it is\n  cheap and also searches the tools' descriptions) before your own knowledge, the\n  web or the files",
			"first call tool_search with a\n  keyword from the question",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s: missing %q", tc.name, want)
			}
		}
		for _, stale := range []string{"For current fact, answer from what you", "- Web search: invoke web_search", "{{"} {
			if strings.Contains(p, stale) {
				t.Errorf("%s: the prompt still says %q", tc.name, stale)
			}
		}
		// Owned by the tool's description, never copied into the prompt.
		if strings.Contains(p, "weather_now") || strings.Contains(p, "Servers and their tools") {
			t.Errorf("%s: tool names or descriptions in the prompt", tc.name)
		}
	}
}

// Without deferred tools the prompt is byte for byte what it was before the
// connected-services ordering, so existing sessions keep their cached prefix.
func TestPromptUnchangedWithoutDeferredTools(t *testing.T) {
	ws := t.TempDir()
	const (
		current = "check the web if you have a\n  web tool (see below); if not, answer from what you know and say it is unchecked."
		search  = "- Web search: invoke web_search on your own judgement whenever the answer\n  depends on information you do not reliably have: current versions, recent releases,\n  changing APIs, anything post-cutoff, or a specific fact you would otherwise hedge\n  about. Needing to search is not a failure; guessing when you could have checked is.\n  You do not need permission, and the user should not have to ask."
		fetch   = "- Web pages: web_fetch reads one page in full — a URL the user gives, a page at\n  an address you know, or a search result whose snippet is not enough."
		noWeb   = "- The web: this session has no web tool. For current fact, answer from what you\n  know and say that you could not check it."
	)
	for _, tc := range []struct {
		tools []string
		web   string
	}{
		{[]string{"read", "web_search", "web_fetch"}, search + "\n" + fetch},
		{[]string{"web_search"}, search},
		{[]string{"web_fetch"}, fetch},
		{[]string{"read"}, noWeb},
	} {
		want := strings.NewReplacer("{{current}}", current, "{{web}}", tc.web, "{{tools}}", "").Replace(CorePrompt)
		p := BuildSystemPrompt(BuildOptions{Profile: "main", Workspace: ws, Tools: tc.tools})
		if !strings.HasPrefix(p, want) {
			t.Errorf("%v: the prompt changed without deferred tools", tc.tools)
		}
		if strings.Contains(p, "tool_search") || strings.Contains(p, "Connected services") {
			t.Errorf("%v: the prompt mentions deferred tools without them", tc.tools)
		}
	}
}
