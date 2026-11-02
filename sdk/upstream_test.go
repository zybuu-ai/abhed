package abhed_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// The upstream proxy is process-wide: an agent whose configuration names a
// different one than an agent already built in this process is refused,
// never run through the other's proxy or none.
func TestSecondDifferentUpstreamProxyRefused(t *testing.T) {
	t.Cleanup(egress.ResetUpstreamForTest)
	egress.ResetUpstreamForTest()
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(url string) {
		body := `{"network":{"upstream_proxy":{"url":"` + url + `"}}}`
		if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(home, ".abhed", "config.json")) })
	newAgent := func() (*abhed.Agent, error) {
		dir := t.TempDir()
		return abhed.New(context.Background(), abhed.Options{Workspace: dir, ConfigDir: dir,
			Provider: &abhed.Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}})
	}
	write("http://proxy-a.corp.test:3128")
	a, err := newAgent()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if up := egress.CurrentUpstream(); up == nil || up.Authority() != "proxy-a.corp.test:3128" {
		t.Fatalf("in force: %+v", up)
	}
	b, err := newAgent()
	if err != nil {
		t.Fatalf("the same proxy again: %v", err)
	}
	b.Close()
	write("http://proxy-b.corp.test:3128")
	if c, err := newAgent(); !errors.Is(err, egress.ErrUpstreamConflict) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("a different proxy in the same process: %v", err)
	}
	if up := egress.CurrentUpstream(); up.Authority() != "proxy-a.corp.test:3128" {
		t.Fatalf("the first agent's proxy changed: %+v", up)
	}
}
