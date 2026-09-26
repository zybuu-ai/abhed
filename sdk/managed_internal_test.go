package abhed

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

var testProvider = &Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}

// managedFile points the managed path at a file holding body for one test.
// An empty body means no managed file at all.
func managedFile(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
	t.Setenv("HOME", t.TempDir())
}

// newAgents builds an agent, once without a config file and once with one,
// since the managed file must bind either way.
func newAgents(t *testing.T, opts Options) ([]*Agent, []error) {
	t.Helper()
	var as []*Agent
	var errs []error
	for _, dir := range []string{"", t.TempDir()} {
		o := opts
		o.Workspace, o.ConfigDir, o.Provider = t.TempDir(), dir, testProvider
		a, err := New(context.Background(), o)
		if a != nil {
			t.Cleanup(a.Close)
		}
		as, errs = append(as, a), append(errs, err)
	}
	return as, errs
}

func wantRefused(t *testing.T, errs []error, key string) {
	t.Helper()
	for i, err := range errs {
		var me *config.ManagedError
		if !errors.As(err, &me) || me.Key != key {
			t.Fatalf("case %d: want %s refused, got %v", i, key, err)
		}
	}
}

func decide(a *Agent, tool, command string) policy.Decision {
	args, _ := json.Marshal(map[string]string{"command": command})
	return a.loop.Policy.Evaluate(tool, true, args).Decision
}

func TestManagedRefusesBypass(t *testing.T) {
	managedFile(t, `{"permissions": {"deny": ["bash(curl*)"]}}`)
	_, errs := newAgents(t, Options{Mode: "bypass"})
	wantRefused(t, errs, "permissions.mode")
	if !strings.Contains(errs[0].Error(), "bypass") {
		t.Errorf("the error should say why: %v", errs[0])
	}

	// The engine is marked managed, so a bypass mode from a lower file is refused there.
	as, errs := newAgents(t, Options{Mode: "auto"})
	for i, a := range as {
		if errs[i] != nil || !a.loop.Policy.Managed {
			t.Fatalf("case %d: the engine is not marked managed: %v", i, errs[i])
		}
	}
}

func TestManagedModeIsPinned(t *testing.T) {
	managedFile(t, `{"permissions": {"mode": "default"}}`)
	for _, m := range []string{"auto", "accept-edits"} {
		_, errs := newAgents(t, Options{Mode: m})
		wantRefused(t, errs, "permissions.mode")
	}
	for m, want := range map[string]policy.Mode{"": policy.ModeDefault, "default": policy.ModeDefault, "plan": policy.ModePlan} {
		as, errs := newAgents(t, Options{Mode: m})
		for i, a := range as {
			if errs[i] != nil || a.loop.Policy.Mode != want {
				t.Errorf("%q case %d: %v, mode %v", m, i, errs[i], a.loop.Policy.Mode)
			}
		}
	}
}

func TestManagedSyntaxCheckIsPinned(t *testing.T) {
	managedFile(t, `{"tools": {"syntax_check": "report"}}`)
	_, errs := newAgents(t, Options{SyntaxCheck: "off"})
	wantRefused(t, errs, "tools.syntax_check")
	for opt, want := range map[string]tools.SyntaxMode{"": tools.SyntaxReport, "refuse": tools.SyntaxRefuse} {
		as, errs := newAgents(t, Options{SyntaxCheck: opt})
		for i, a := range as {
			if errs[i] != nil || a.loop.Session.Syntax != want {
				t.Errorf("%q case %d: %v, syntax %v", opt, i, errs[i], a.loop.Session.Syntax)
			}
		}
	}
}

func TestEmbedderDenyRulesAreAdditive(t *testing.T) {
	managedFile(t, `{"permissions": {"deny": ["bash(curl*)"], "ask": ["bash(git push*)"]}}`)
	as, errs := newAgents(t, Options{Mode: "auto", Deny: []string{"bash(wget*)"}, Allow: []string{"bash(git*)"}})
	for i, a := range as {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		for cmd, want := range map[string]policy.Decision{
			"curl http://x": policy.Deny, "wget http://x": policy.Deny,
			"git push origin": policy.Ask, "git status": policy.Allow,
		} {
			if got := decide(a, "bash", cmd); got != want {
				t.Errorf("case %d: %s decided %v, want %v", i, cmd, got, want)
			}
		}
	}
}

func TestManagedAllowAndTurnsBind(t *testing.T) {
	managedFile(t, `{"permissions": {"allow": ["bash(go test*)"]}, "limits": {"max_turns": 10}}`)
	_, errs := newAgents(t, Options{Allow: []string{"bash(*)"}})
	wantRefused(t, errs, "permissions.allow")
	_, errs = newAgents(t, Options{MaxTurns: 50})
	wantRefused(t, errs, "limits.max_turns")
	for opt, want := range map[int]int{0: 10, 4: 4} {
		as, errs := newAgents(t, Options{MaxTurns: opt})
		for i, a := range as {
			if errs[i] != nil || a.loop.Config.MaxTurns != want {
				t.Errorf("%d case %d: %v, turns %d", opt, i, errs[i], a.loop.Config.MaxTurns)
			}
		}
	}
}

// A managed sandbox setting wraps bash, where otherwise it runs bare. This
// proves the wrapper is installed; internal/sandbox tests the isolation.
func TestManagedSandboxBinds(t *testing.T) {
	managedFile(t, `{"sandbox": {"min_tier": "none"}}`)
	as, errs := newAgents(t, Options{})
	for i, a := range as {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		b, _ := a.registry.Get("bash")
		if b.(tools.Bash).Sandbox == nil {
			t.Errorf("case %d: bash is not sandboxed", i)
		}
	}
}

// Without a managed file, Options override the configuration as they always did.
func TestNoManagedLayerIsUnchanged(t *testing.T) {
	managedFile(t, "")
	as, errs := newAgents(t, Options{Mode: "bypass", SyntaxCheck: "off", MaxTurns: 500, Allow: []string{"bash(*)"}})
	for i, a := range as {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if a.loop.Policy.Managed || a.loop.Policy.Mode != policy.ModeBypass ||
			a.loop.Session.Syntax != tools.SyntaxOff || a.loop.Config.MaxTurns != 500 {
			t.Errorf("case %d: overrides not applied", i)
		}
		if decide(a, "bash", "make") != policy.Allow {
			t.Errorf("case %d: bypass should allow", i)
		}
		b, _ := a.registry.Get("bash")
		if b.(tools.Bash).Sandbox != nil {
			t.Errorf("case %d: no sandbox was configured, yet bash has one", i)
		}
	}
}

// Options.Sandbox builds the configured tier, and a tier the host cannot give
// fails New rather than leaving bash on the host.
func TestSandboxOptionBuildsTheConfiguredTier(t *testing.T) {
	managedFile(t, "")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".abhed", "config.json"), []byte(`{"sandbox": {"min_tier": "vm"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{Workspace: dir, ConfigDir: dir, Provider: testProvider, Sandbox: true}
	a, err := New(context.Background(), opts)
	if err == nil {
		a.Close()
		t.Skip("this host can give the vm tier")
	}
	if !strings.Contains(err.Error(), "minimum tier") {
		t.Fatalf("want the tier refused, got %v", err)
	}
	opts.Sandbox = false
	a, err = New(context.Background(), opts)
	if err != nil {
		t.Fatalf("without Sandbox the configured tier is not built: %v", err)
	}
	a.Close()
}
