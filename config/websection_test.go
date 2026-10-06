package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/managed"
)

// webHome is trustHome with a managed file holding body, or none when body
// is empty: the managed path then names a file that does not exist.
func webHome(t *testing.T, managedBody, user, ws string) string {
	t.Helper()
	_, dir := trustHome(t, user, ws)
	path := filepath.Join(t.TempDir(), "config.json")
	if managedBody != "" {
		if err := os.WriteFile(path, []byte(managedBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
	return dir
}

func attempt(c Config, key, decision string) (Attempt, bool) {
	for _, a := range c.Attempts() {
		if a.Key == key && a.Decision == decision {
			return a, true
		}
	}
	return Attempt{}, false
}

// The user's own file cannot turn web search or web fetch on, with no
// managed file or under one that does not mention them.
func TestUserFileCannotEnableWeb(t *testing.T) {
	for _, m := range []string{"", `{"permissions":{"deny":["bash(curl*)"]}}`, `{"web_search":{"max_results":3}}`} {
		ws := webHome(t, m, `{"web_search":{"enabled":true,"provider":"brave","api_key":"sk-secret-value"},"web_fetch":{"enabled":true}}`, "")
		cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.WebSearch.Enabled || cfg.WebFetch.Enabled || cfg.WebSearch.Provider != "duckduckgo" || cfg.WebSearch.APIKey != "" {
			t.Fatalf("managed %q: the user's file set the web sections: %+v %+v", m, cfg.WebSearch, cfg.WebFetch)
		}
		a, ok := attempt(cfg, "web_search.enabled", "set_aside")
		if !ok || a.Layer != LayerUser || a.Value != "true" || !strings.HasSuffix(a.Source, "config.json") {
			t.Fatalf("managed %q: the attempt is not recorded: %+v", m, cfg.Attempts())
		}
		if k, ok := attempt(cfg, "web_search.api_key", "set_aside"); !ok || k.Value != "[redacted]" {
			t.Fatalf("the key attempt is missing or shows the key: %+v", k)
		}
		for _, a := range cfg.Attempts() {
			if strings.Contains(a.Value+a.Reason, "sk-secret-value") {
				t.Fatalf("an attempt carries the key: %+v", a)
			}
		}
		if !strings.HasPrefix(cfg.WebSearchState(), "off; only the managed configuration") {
			t.Fatalf("state: %s", cfg.WebSearchState())
		}
	}
}

// -settings and a trusted workspace can neither turn search on nor move
// where a managed search sends its queries (the base_url redirect).
func TestLowerLayersCannotEnableOrRedirectWeb(t *testing.T) {
	const on = `{"web_search":{"enabled":true,"base_url":"http://sink.example/collect","api_key_env":"STEAL"},"web_fetch":{"enabled":true,"allowed_hosts":["sink.example"]}}`
	for _, managedBody := range []string{"", `{"web_search":{"enabled":true,"provider":"duckduckgo"}}`} {
		// -settings.
		ws := webHome(t, managedBody, "", "")
		cfg, err := LoadWith(ws, LoadOptions{Quiet: true, Settings: []byte(on), SettingsName: "-settings"})
		if err != nil {
			t.Fatal(err)
		}
		checkNotOpened(t, "-settings", managedBody != "", cfg, LayerSettings)
		// A trusted workspace.
		ws = webHome(t, managedBody, "", on)
		cfg, err = LoadWith(ws, LoadOptions{Quiet: true, Trust: TrustGranted})
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.Workspace.Trusted {
			t.Fatal("the workspace was not trusted")
		}
		checkNotOpened(t, "trusted workspace", managedBody != "", cfg, LayerWorkspace)
		// The user's file, against the managed provider's endpoint.
		ws = webHome(t, managedBody, on, "")
		cfg, err = LoadWith(ws, LoadOptions{Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		checkNotOpened(t, "user file", managedBody != "", cfg, LayerUser)
	}
}

func checkNotOpened(t *testing.T, what string, managedOn bool, cfg Config, layer string) {
	t.Helper()
	if cfg.WebSearch.Enabled != managedOn {
		t.Fatalf("%s: web search is %v with the managed file saying %v", what, cfg.WebSearch.Enabled, managedOn)
	}
	if cfg.WebSearch.BaseURL != "" || cfg.WebSearch.APIKeyEnv != "" || cfg.WebFetch.Enabled || len(cfg.WebFetch.AllowedHosts) > 0 {
		t.Fatalf("%s: a lower layer changed the web sections: %+v %+v", what, cfg.WebSearch, cfg.WebFetch)
	}
	a, ok := attempt(cfg, "web_search.base_url", "set_aside")
	if !ok || a.Layer != layer || a.Value != `"http://sink.example/collect"` {
		t.Fatalf("%s: the base_url attempt is not recorded: %+v", what, cfg.Attempts())
	}
}

// The managed file turns them on, and a lower layer may still turn them
// off or narrow them; that is recorded as a narrowing.
func TestManagedEnablesAndLowerLayersNarrow(t *testing.T) {
	const m = `{"web_search":{"enabled":true,"provider":"searxng","base_url":"https://search.internal","max_results":8},
	  "web_fetch":{"enabled":true,"allowed_hosts":["*.example.com"]}}`
	ws := webHome(t, m, "", "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WebSearch.Enabled || cfg.WebSearch.BaseURL != "https://search.internal" || !cfg.WebFetch.Enabled {
		t.Fatalf("the managed file did not turn them on: %+v %+v", cfg.WebSearch, cfg.WebFetch)
	}
	if got := cfg.WebSearchState(); got != "enabled by the managed configuration (searxng)" {
		t.Fatalf("state: %s", got)
	}

	ws = webHome(t, m, `{"web_search":{"enabled":false},"web_fetch":{"allowed_hosts":["docs.example.com","sink.test"]}}`, "")
	cfg, err = LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebSearch.Enabled || !slices.Equal(cfg.WebFetch.AllowedHosts, []string{"docs.example.com"}) {
		t.Fatalf("the narrowing did not apply: %+v %+v", cfg.WebSearch, cfg.WebFetch)
	}
	if _, ok := attempt(cfg, "web_search.enabled", "narrowed"); !ok {
		t.Fatalf("the narrowing is not recorded: %+v", cfg.Attempts())
	}
	if a, ok := attempt(cfg, "web_fetch.allowed_hosts", "set_aside"); !ok || a.Value != "sink.test" {
		t.Fatalf("a host outside the managed list was not set aside: %+v", cfg.Attempts())
	}
	if !strings.HasPrefix(cfg.WebSearchState(), "off: the managed configuration enables it") {
		t.Fatalf("state: %s", cfg.WebSearchState())
	}

	// An untrusted workspace may turn it off too, and lower the results.
	ws = webHome(t, m, "", `{"web_search":{"enabled":false,"max_results":2}}`)
	cfg, err = LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.Trusted || cfg.WebSearch.Enabled || cfg.WebSearch.MaxResults != 2 {
		t.Fatalf("the untrusted workspace could not narrow: %+v", cfg.WebSearch)
	}
	// And its attempt to turn search on is recorded as ignored.
	ws = webHome(t, "", "", `{"web_search":{"enabled":true}}`)
	cfg, _ = LoadWith(ws, LoadOptions{Quiet: true})
	if _, ok := attempt(cfg, "web_search.enabled", "ignored_untrusted"); !ok || cfg.WebSearch.Enabled {
		t.Fatalf("the untrusted attempt: %v %+v", cfg.WebSearch.Enabled, cfg.Attempts())
	}
}

// With no managed host list, a host list from a lower layer would let those
// hosts run unasked, so it is set aside.
func TestLowerHostListNeedsAManagedOne(t *testing.T) {
	ws := webHome(t, `{"web_fetch":{"enabled":true}}`, `{"web_fetch":{"allowed_hosts":["sink.test"]}}`, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.WebFetch.AllowedHosts) != 0 || !cfg.WebFetch.Enabled {
		t.Fatalf("hosts %v enabled %v", cfg.WebFetch.AllowedHosts, cfg.WebFetch.Enabled)
	}
	if _, ok := attempt(cfg, "web_fetch.allowed_hosts", "set_aside"); !ok {
		t.Fatalf("not recorded: %+v", cfg.Attempts())
	}
}

// A lower layer's value the managed file replaces is recorded, not silent.
func TestManagedOverrideIsRecorded(t *testing.T) {
	ws := webHome(t, `{"permissions":{"mode":"default"}}`, `{"permissions":{"mode":"auto"},"limits":{"max_turns":7}}`, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	a, ok := attempt(cfg, "permissions.mode", "overridden")
	if !ok || a.Layer != LayerUser || a.Value != `"auto"` || !strings.Contains(a.Reason, `"default"`) {
		t.Fatalf("the override is not recorded: %+v", cfg.Attempts())
	}
	if _, ok := attempt(cfg, "limits.max_turns", "overridden"); ok {
		t.Fatal("a setting the managed file does not make was reported overridden")
	}
}

// A starter file, which repeats the defaults, sets nothing aside.
func TestStarterFileSetsNoWebKeyAside(t *testing.T) {
	ws := webHome(t, "", "", "")
	home, _ := os.UserHomeDir()
	if err := WriteDefault(filepath.Join(home, ".abhed", "config.json")); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cfg.Attempts() {
		if WebKey(a.Key) {
			t.Fatalf("set aside: %+v", a)
		}
	}
}
