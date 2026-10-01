package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/webfetch"
)

// Every surface builds its web tools through the tool set, so this is where
// the defaults and the link between the two tools are held.
func TestWebToolsAreOffUntilEnabledAndKnowOfEachOther(t *testing.T) {
	buildWebTools := func(cfg config.Config) []tools.Tool {
		set := toolset.Build(t.Context(), cfg, toolset.Options{Workspace: t.TempDir(),
			Parts: toolset.WebSearch | toolset.WebFetch,
			Vault: secrets.Open(filepath.Join(t.TempDir(), "s.json"))})
		t.Cleanup(set.Close)
		var out []tools.Tool
		for _, n := range set.Registry.Names() {
			if strings.HasPrefix(n, "web_") {
				tool, _ := set.Registry.Get(n)
				out = append(out, tool)
			}
		}
		return out
	}
	cfg := config.Default()
	if got := buildWebTools(cfg); len(got) != 0 {
		t.Fatalf("web tools on by default: %d", len(got))
	}

	cfg.WebSearch.Enabled = true
	got := buildWebTools(cfg)
	if len(got) != 1 || got[0].Name() != "web_search" || strings.Contains(got[0].Description(), "web_fetch") {
		t.Fatalf("search alone: %v", names(got))
	}

	cfg.WebFetch.Enabled = true
	cfg.WebFetch.AllowedHosts = []string{"docs.python.org"}
	got = buildWebTools(cfg)
	if len(got) != 2 || got[0].Name() != "web_search" || !strings.Contains(got[0].Description(), "web_fetch") {
		t.Fatalf("both: %v", names(got))
	}
	f, ok := got[1].(*webfetch.Tool)
	if !ok || f.Secrets == nil || len(f.AllowedHosts) != 1 {
		t.Fatalf("web_fetch built without its secret check or allowlist: %#v", got[1])
	}

	cfg.WebSearch.Enabled = false
	if got := buildWebTools(cfg); len(got) != 1 || got[0].Name() != "web_fetch" {
		t.Fatalf("fetch alone: %v", names(got))
	}
}

func names[T interface{ Name() string }](ts []T) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

// With no host list web_fetch asks in default and auto modes; with one, the
// tool itself refuses other hosts and the listed ones run unasked.
func TestWebFetchAsksWithoutAHostList(t *testing.T) {
	args := []byte(`{"url":"https://example.com/"}`)
	cfg := config.Default()
	cfg.WebFetch.Enabled = true
	for _, mode := range []policy.Mode{policy.ModeDefault, policy.ModeAuto} {
		pol := policy.New(mode)
		pol.AskReadOnly = webfetch.AskReadOnly(cfg.WebFetch.Enabled, cfg.WebFetch.AllowedHosts)
		if got := pol.Evaluate("web_fetch", false, args); got.Decision != policy.Ask || !strings.Contains(got.Reason, "no allowed_hosts") {
			t.Errorf("%s: %+v", mode, got)
		}
	}
	cfg.WebFetch.AllowedHosts = []string{"example.com"}
	pol := policy.New(policy.ModeDefault)
	pol.AskReadOnly = webfetch.AskReadOnly(cfg.WebFetch.Enabled, cfg.WebFetch.AllowedHosts)
	if got := pol.Evaluate("web_fetch", false, args); got.Decision != policy.Allow {
		t.Errorf("with a host list: %+v", got)
	}
}
