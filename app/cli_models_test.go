package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
)

type fakeAdapter struct {
	name string
	err  error
	n    int
}

func (f *fakeAdapter) Name() string                           { return f.name }
func (f *fakeAdapter) Profile() model.Profile                 { return model.Profile{Name: f.name} }
func (f *fakeAdapter) CountTokens(model.Request) (int, error) { return 0, nil }
func (f *fakeAdapter) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	f.n++
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan model.Chunk)
	close(ch)
	return ch, nil
}

func TestFallbackAdapterMovesOnceAndRecords(t *testing.T) {
	a := &fakeAdapter{name: "A", err: &model.StatusError{Status: 403}}
	b := &fakeAdapter{name: "B"}
	var moves []string
	f := newFallbackAdapter("a", a, []string{"b"}, func(n string) (model.Adapter, error) { return b, nil })
	f.SetRecord(func(from, to, why string) { moves = append(moves, from+">"+to+":"+why) })
	for range 2 {
		if _, err := f.Complete(context.Background(), model.Request{}); err != nil {
			t.Fatal(err)
		}
	}
	if a.n != 1 || b.n != 2 || f.Current() != "b" || f.Profile().Name != "B" {
		t.Fatalf("a %d b %d current %s", a.n, b.n, f.Current())
	}
	if !slices.Equal(moves, []string{"a>b:refusing access (403)"}) {
		t.Fatalf("moves %q", moves)
	}
	// A request the model rejected is not the model being unavailable.
	bad := &fakeAdapter{name: "A", err: &model.StatusError{Status: 400}}
	g := newFallbackAdapter("a", bad, []string{"b"}, func(string) (model.Adapter, error) { return b, nil })
	if _, err := g.Complete(context.Background(), model.Request{}); err == nil || g.Current() != "a" {
		t.Fatalf("a 400 moved: %v %s", err, g.Current())
	}
}

func TestFallbackReason(t *testing.T) {
	for _, c := range []struct {
		err  error
		move bool
	}{
		{&model.StatusError{Status: 401}, true}, {&model.StatusError{Status: 404}, true},
		{&model.StatusError{Status: 503}, true}, {&model.StatusError{Status: 400}, false},
		{errors.New("context length exceeded"), false},
	} {
		if _, move := fallbackReason(c.err); move != c.move {
			t.Errorf("%v: %v", c.err, move)
		}
	}
}

func TestFallbackChain(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Default = "a"
	cfg.Model.Providers = map[string]config.ProviderConfig{"a": {}, "b": {}, "c": {}}
	cfg.SetKeys = []string{"model.providers.a", "model.providers.b", "model.providers.c"}
	cfg.Model.Fallback = []string{"c", "b"}
	chain, warn := fallbackChain(cfg, "b,zz,a")
	if !slices.Equal(chain, []string{"b", "c"}) || len(warn) != 1 || !strings.Contains(warn[0], `"zz"`) {
		t.Fatalf("%q %q", chain, warn)
	}
	cfg.ManagedKeys = []string{"model.default"}
	if chain, warn := fallbackChain(cfg, "b"); chain != nil || len(warn) != 1 {
		t.Fatalf("left a managed model: %q %q", chain, warn)
	}
}

func TestHostedLabel(t *testing.T) {
	for in, want := range map[string]string{"http://localhost:1/v1": "local", "http://127.0.0.1/v1": "local",
		"http://[::1]:8/v1": "local", "https://api.example.com/v1": "hosted", "": "hosted"} {
		if got := hostedLabel(in); got != want {
			t.Errorf("%q: %s", in, got)
		}
	}
}

func TestConfigKeysWiden(t *testing.T) {
	find := func(p string) configKey {
		for _, k := range configKeys {
			if k.path == p {
				return k
			}
		}
		t.Fatalf("no %s", p)
		return configKey{}
	}
	for _, c := range []struct {
		path, from, to string
		widens         bool
	}{
		{"permissions.mode", "default", "plan", false}, {"permissions.mode", "default", "bypass", true},
		{"sandbox.allow_network", "false", "true", true}, {"sandbox.allow_network", "true", "false", false},
		{"limits.max_turns", "100", "20", false}, {"limits.max_turns", "100", "0", true}, {"limits.max_turns", "10", "50", true},
		{"tools.syntax_check", "refuse", "off", true}, {"tools.syntax_check", "off", "refuse", false},
		{"statusline.command", "", "echo hi", true}, {"statusline.command", "echo hi", "", false},
	} {
		if got := find(c.path).widens(c.from, c.to); got != c.widens {
			t.Errorf("%s %s→%s: %v", c.path, c.from, c.to, got)
		}
	}
}

func TestWriteUserSettingKeepsTheRest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := filepath.Join(home, ".abhed", "config.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"model":{"default":"x"},"permissions":{"deny":["bash(curl*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeUserSetting("permissions.mode", "plan"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	var doc map[string]map[string]any
	if err := json.Unmarshal(data, &doc); err != nil || doc["model"]["default"] != "x" || doc["permissions"]["mode"] != "plan" || doc["permissions"]["deny"] == nil {
		t.Fatalf("%v %s", err, data)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	_ = os.WriteFile(file, []byte(`{broken`), 0o600)
	if _, err := writeUserSetting("permissions.mode", "plan"); err == nil {
		t.Fatal("a broken file was overwritten")
	}
}

// Under a managed model.default the chain is the managed model.fallback
// exactly: -fallback-model names are left out with a warning.
func TestFallbackChainUnderAManagedModel(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Default = "a"
	cfg.Model.Providers = map[string]config.ProviderConfig{"a": {}, "b": {}, "c": {}}
	cfg.SetKeys = []string{"model.providers.a", "model.providers.b", "model.providers.c"}
	cfg.Model.Fallback = []string{"b"}
	cfg.ManagedKeys = []string{"model.default", "model.fallback"}
	chain, warn := fallbackChain(cfg, "c")
	if !slices.Equal(chain, []string{"b"}) || len(warn) != 1 || !strings.Contains(warn[0], "-fallback-model") {
		t.Fatalf("%q %q", chain, warn)
	}
	if chain, warn := fallbackChain(cfg, ""); !slices.Equal(chain, []string{"b"}) || len(warn) != 0 {
		t.Fatalf("%q %q", chain, warn)
	}
}

// -model is refused under a managed model.default, as /model is.
func TestModelFlagUnderAManagedModel(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Default = "a"
	if got, err := modelFlag(cfg, "b"); err != nil || got.Model.Default != "b" {
		t.Fatalf("unmanaged: %v %q", err, got.Model.Default)
	}
	cfg.ManagedKeys = []string{"model.default"}
	if _, err := modelFlag(cfg, "b"); err == nil {
		t.Fatal("-model left a managed model")
	}
	if got, err := modelFlag(cfg, "a"); err != nil || got.Model.Default != "a" {
		t.Fatalf("naming the managed model: %v", err)
	}
}

// /model refuses to leave a managed model.default.
func TestSwitchModelRefusedUnderAManagedModel(t *testing.T) {
	cfg := config.Default()
	cfg.Model.Default = "a"
	cfg.Model.Providers = map[string]config.ProviderConfig{
		"a": {Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", Model: "m"},
		"b": {Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", Model: "m"}}
	cfg.SetKeys = []string{"model.providers.a", "model.providers.b"}
	cfg.ManagedKeys = []string{"model.default"}
	e, _ := configEnv(cfg, "")
	if err := switchModel(context.Background(), e, "b"); err == nil || !strings.Contains(err.Error(), "managed") {
		t.Fatalf("switched: %v", err)
	}
	if e.st.appCfg.Model.Default != "a" {
		t.Fatal("the default moved")
	}
}
