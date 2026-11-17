package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An org policy may turn plugins off, require signatures, list publishers
// and bundles, and name its own server as a source; it never names another
// server or a file on the machine.
func TestOrgPolicyPluginKeys(t *testing.T) {
	doc, err := decodeDoc([]byte(`{"plugins":{"disabled":true,"require_signature":true,"org_source":true,
		"allowed_publishers":[{"identity":"r@x","issuer":"https://i"}],"allowed_bundles":["` + strings.Repeat("a", 64) + `"],
		"sources":[{"name":"evil","url":"https://evil.example/index"}],"trusted_root":"/tmp/root.json"}}`))
	if err != nil {
		t.Fatal(err)
	}
	below, ws := Default(), Default()
	out := stripOrg(doc, orgStrip{below: &below, ws: &ws, pinned: true})
	aside := map[string]bool{}
	for _, o := range out {
		aside[o.key] = true
	}
	if !aside["plugins.sources"] || !aside["plugins.trusted_root"] || len(out) != 2 {
		t.Fatalf("set aside %v", out)
	}
	// Loosening is refused: false is not the org's to say.
	doc, _ = decodeDoc([]byte(`{"plugins":{"disabled":false,"require_signature":false}}`))
	if out := stripOrg(doc, orgStrip{below: &below, ws: &ws}); len(out) != 2 {
		t.Fatalf("an org loosened plugins: %v", out)
	}
}

func TestPluginPolicyOrgSource(t *testing.T) {
	c := Default()
	c.Plugins.OrgSource = true
	if len(c.PluginPolicy().Sources) != 0 {
		t.Fatal("an org source with no org server")
	}
	c.Org.URL = "https://abhed.acme.example/"
	src := c.PluginPolicy().Sources
	if len(src) != 1 || src[0].Name != OrgSourceName || src[0].URL != "https://abhed.acme.example/v1/plugins/index" {
		t.Fatalf("sources %+v", src)
	}
}

// The person's own file, like a workspace's, never sets the plugin policy.
func TestUserFileCannotSetPluginPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"),
		[]byte(`{"plugins":{"allowed_bundles":["`+strings.Repeat("b", 64)+`"],"sources":[{"name":"x","url":"https://x/i"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Plugins.AllowedBundles) != 0 || len(cfg.Plugins.Sources) != 0 {
		t.Fatalf("the user's file set the plugin policy: %+v", cfg.Plugins)
	}
}
