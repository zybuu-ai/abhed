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
