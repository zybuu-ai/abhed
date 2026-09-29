package abhed_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// An untrusted ConfigDir file that names its own model makes New fail rather
// than run on another model, unless the caller says which way to go.
func TestNewRefusesAnIgnoredModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(config.TrustEnv, "")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := `{"model":{"default":"gpu","providers":{"gpu":{"type":"openai-compatible","base_url":"http://127.0.0.1:1/v1","model":"m","context_window":8192}}}}`
	if err := os.WriteFile(filepath.Join(dir, ".abhed", "config.json"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := abhed.New(ctx, abhed.Options{Workspace: dir, ConfigDir: dir}); !errors.Is(err, abhed.ErrUntrustedModel) {
		t.Fatalf("New: %v, want ErrUntrustedModel", err)
	}
	for name, opts := range map[string]abhed.Options{
		"allow default": {Workspace: dir, ConfigDir: dir, AllowDefaultModel: true},
		"granted":       {Workspace: dir, ConfigDir: dir, WorkspaceTrust: config.TrustGranted},
		"refused":       {Workspace: dir, ConfigDir: dir, WorkspaceTrust: config.TrustRefused},
		"provider":      {Workspace: dir, ConfigDir: dir, Provider: &abhed.Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m", ContextWindow: 8192}},
	} {
		a, err := abhed.New(ctx, opts)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if st := a.WorkspaceTrust(); (name == "granted") != st.Trusted {
			t.Errorf("%s: trusted %v", name, st.Trusted)
		}
		a.Close()
	}
}
