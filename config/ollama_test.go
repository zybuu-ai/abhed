package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/managed"
)

// model.ollama.assist: ask by default, an untrusted workspace may only
// make it do less, and a managed file that sets it holds it.
func TestOllamaAssist(t *testing.T) {
	_, ws := trustHome(t, `{"model":{"ollama":{"assist":"local"}}}`, `{"model":{"ollama":{"assist":"ask"}}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.OllamaAssist(); got != "local" {
		t.Fatalf("an untrusted workspace widened local to %s", got)
	}
	writeConfig(t, ws, `{"model":{"ollama":{"assist":"off"}}}`)
	if cfg, _ = LoadWith(ws, LoadOptions{Quiet: true}); cfg.OllamaAssist() != "off" {
		t.Fatalf("an untrusted workspace could not narrow to off: %s", cfg.OllamaAssist())
	}

	_, ws = trustHome(t, "", "")
	if cfg, _ = LoadWith(ws, LoadOptions{Quiet: true}); cfg.OllamaAssist() != "ask" {
		t.Fatalf("default %s", cfg.OllamaAssist())
	}

	_, ws = trustHome(t, `{"model":{"ollama":{"assist":"ask"}}}`, "")
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(`{"model":{"ollama":{"assist":"off"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
	cfg, err = LoadWith(ws, LoadOptions{Quiet: true, Settings: []byte(`{"model":{"ollama":{"assist":"ask"}}}`), SettingsName: "-settings"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OllamaAssist() != "off" || !cfg.ManagedSets("model.ollama.assist") {
		t.Fatalf("the managed off did not hold: %s (managed keys %v)", cfg.OllamaAssist(), cfg.ManagedKeys)
	}

	bad := Default()
	bad.Model.Ollama.Assist = "auto"
	if err := bad.Validate(); err == nil {
		t.Fatal("auto passed validation")
	}
}
