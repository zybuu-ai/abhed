package abhed

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/secrets"
)

type countClose struct{ n *atomic.Int32 }

func (c countClose) Close() error { c.n.Add(1); return nil }

// Closing an agent closes what its tools kept for its session, a login's
// clients or a connected host, as deleting a console session does.
func TestCloseReleasesTheSessionsScopedState(t *testing.T) {
	p := &Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}
	a, err := New(context.Background(), Options{Workspace: t.TempDir(), Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Int32
	type key struct{}
	a.loop.Session.Scoped(key{}, func() any { return countClose{&closed} })
	a.Close()
	if closed.Load() != 1 {
		t.Fatalf("scoped state closed %d times on Close, want 1", closed.Load())
	}
}

// With the configured tools, k8s_login and ssh_connect read the same secrets
// store as the command line, so the names the model may use are listed.
func TestConfiguredLoginsReadTheSecretsStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := secrets.Default().Set("OCP_TOKEN", "sha256~a-stored-token"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"k8s":{"enabled":true,"clusters":[{"name":"prod","server":"https://api.prod.example:6443"}]}}`
	if err := os.WriteFile(filepath.Join(dir, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}
	a, err := New(context.Background(), Options{Workspace: dir, ConfigDir: dir, Provider: p,
		WorkspaceTrust: config.TrustGranted, ConfiguredTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	login, ok := a.registry.Get("k8s_login")
	if !ok {
		t.Fatal("no k8s_login with k8s enabled")
	}
	if !strings.Contains(login.Description(), "Stored secrets: OCP_TOKEN") {
		t.Fatalf("k8s_login does not read the secrets store: %s", login.Description())
	}
}
