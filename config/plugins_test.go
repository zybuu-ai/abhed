package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/orgpolicy"
	"github.com/zybuu-ai/abhed/plugin"
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
	// What an org URL carries in secret never reaches the source's URL,
	// which fetch errors and listings print.
	c.OrgPolicy.URL = "https://u:SECRET@abhed.acme.example/?token=SECRET#SECRET"
	src = c.PluginPolicy().Sources
	if len(src) != 1 || src[0].URL != "https://abhed.acme.example/v1/plugins/index" {
		t.Fatalf("sources from a secret-bearing org URL %+v", src)
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

// The machine's managed lists and the org policy's are layers: a bundle
// must pass both, an org without pinned keys only narrows, and the person's
// own publishers count only within them.
func TestPluginPolicyLayers(t *testing.T) {
	iss := "https://issuer.acme.example"
	a := &plugin.Signer{Identity: "a@acme.example", Issuer: iss}
	b := &plugin.Signer{Identity: "b@acme.example", Issuer: iss}
	x, y := strings.Repeat("1", 64), strings.Repeat("2", 64)
	pub := func(s *plugin.Signer) []plugin.Publisher {
		return []plugin.Publisher{{Identity: s.Identity, Issuer: iss}}
	}
	cfg := func(managed PluginsConfig, org *PluginsConfig, pinned bool) Config {
		c := Default()
		c.Plugins = managed
		if org != nil {
			c.orgPlugins = &orgPlugins{PluginsConfig: *org, pinned: pinned}
		}
		return c
	}
	for _, tc := range []struct {
		name string
		c    Config
		e    plugin.Evidence
		mine []plugin.Publisher
		ok   bool
	}{
		{"managed publishers, org bundles: signed by A, digest not listed", cfg(PluginsConfig{AllowedPublishers: pub(a)}, &PluginsConfig{AllowedBundles: []string{x}}, false),
			plugin.Evidence{Digest: y, Signer: a}, nil, false},
		{"managed publishers, org bundles: X unsigned", cfg(PluginsConfig{AllowedPublishers: pub(a)}, &PluginsConfig{AllowedBundles: []string{x}}, true),
			plugin.Evidence{Digest: x}, nil, false},
		{"managed publishers, org bundles: X signed by A", cfg(PluginsConfig{AllowedPublishers: pub(a)}, &PluginsConfig{AllowedBundles: []string{x}}, false),
			plugin.Evidence{Digest: x, Signer: a}, nil, true},
		{"managed bundles, org publishers: signed by B, digest not listed", cfg(PluginsConfig{AllowedBundles: []string{x}}, &PluginsConfig{AllowedPublishers: pub(b)}, true),
			plugin.Evidence{Digest: y, Signer: b}, nil, false},
		{"unpinned org publishers, the person trusts nobody", cfg(PluginsConfig{}, &PluginsConfig{AllowedPublishers: pub(b)}, false),
			plugin.Evidence{Digest: y, Signer: b}, nil, false},
		{"unpinned org publishers, the person trusts B", cfg(PluginsConfig{}, &PluginsConfig{AllowedPublishers: pub(b)}, false),
			plugin.Evidence{Digest: y, Signer: b}, pub(b), true},
		{"unpinned org publishers, the person trusts A only", cfg(PluginsConfig{}, &PluginsConfig{AllowedPublishers: pub(b)}, false),
			plugin.Evidence{Digest: y, Signer: a}, pub(a), false},
		{"pinned org publishers grant", cfg(PluginsConfig{}, &PluginsConfig{AllowedPublishers: pub(b)}, true),
			plugin.Evidence{Digest: y, Signer: b}, nil, true},
		{"unpinned org bundles do not grant an unpinned digest", cfg(PluginsConfig{}, &PluginsConfig{AllowedBundles: []string{x}}, false),
			plugin.Evidence{Digest: x}, nil, false},
		{"org require_signature binds", cfg(PluginsConfig{}, &PluginsConfig{RequireSignature: true}, false),
			plugin.Evidence{Digest: x, Pin: x}, nil, false},
		{"org disabled binds", cfg(PluginsConfig{}, &PluginsConfig{Disabled: true}, false),
			plugin.Evidence{Digest: x, Signer: a}, pub(a), false},
	} {
		_, err := tc.c.PluginPolicy().Admit(tc.e, tc.mine)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// The org source is trusted only from pinned keys or the machine's file.
	c := cfg(PluginsConfig{}, &PluginsConfig{OrgSource: true}, false)
	c.Org.URL = "https://abhed.acme.example"
	if s := c.PluginPolicy().Sources; len(s) != 1 || s[0].Trusted {
		t.Fatalf("an unpinned org's source: %+v", s)
	}
	c = cfg(PluginsConfig{}, &PluginsConfig{OrgSource: true}, true)
	c.Org.URL = "https://abhed.acme.example"
	if s := c.PluginPolicy().Sources; len(s) != 1 || !s[0].Trusted {
		t.Fatalf("a pinned org's source: %+v", s)
	}
}

// The org's plugins section leaves the merged document, so it never
// combines field by field with the machine's.
func TestTakeOrgPlugins(t *testing.T) {
	doc, _ := decodeDoc([]byte(`{"plugins":{"allowed_bundles":["` + strings.Repeat("1", 64) + `"]},"limits":{"max_turns":3}}`))
	c := Default()
	if err := takeOrgPlugins(&c, doc, false, "org"); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["plugins"]; ok || c.orgPlugins == nil || len(c.orgPlugins.AllowedBundles) != 1 || c.orgPlugins.pinned {
		t.Fatalf("doc %v org %+v", doc, c.orgPlugins)
	}
	bad, _ := decodeDoc([]byte(`{"plugins":{"allowed_publishers":[{"identity":"r@*","issuer":"https://i"}]}}`))
	if err := takeOrgPlugins(&c, bad, false, "org"); err == nil {
		t.Fatal("an email wildcard was taken")
	}
}

// Through a real signed org policy: the machine lists publishers, the org
// lists bundles, and the policy has both as layers rather than a union.
func TestOrgPluginListsAreALayer(t *testing.T) {
	withManaged(t, `{"plugins":{"allowed_publishers":[{"identity":"a@acme.example","issuer":"https://issuer.acme.example"}]}}`)
	f := newOrg(t)
	x := strings.Repeat("1", 64)
	f.write(t, `{"plugins":{"allowed_bundles":["`+x+`"],"require_signature":true}}`, time.Now())
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	pol := cfg.PluginPolicy()
	if len(pol.Lists) != 2 || !pol.RequireSignature || len(cfg.Plugins.AllowedBundles) != 0 {
		t.Fatalf("policy %+v plugins %+v", pol, cfg.Plugins)
	}
	a := &plugin.Signer{Identity: "a@acme.example", Issuer: "https://issuer.acme.example"}
	if _, err := pol.Admit(plugin.Evidence{Digest: strings.Repeat("2", 64), Signer: a}, nil); err == nil {
		t.Fatal("the org's bundle list did not narrow the machine's publisher")
	}
	if _, err := pol.Admit(plugin.Evidence{Digest: x, Signer: a}, nil); err != nil {
		t.Fatalf("a bundle both admit: %v", err)
	}
	if !slices.Contains(cfg.OrgPolicy.Keys, "plugins.allowed_bundles") {
		t.Errorf("org keys %v", cfg.OrgPolicy.Keys)
	}
}

// A pinned org's lists grant only from a fresh, verified policy: stale, it
// still narrows (its lists are kept) but never grants.
func TestStalePinnedOrgPluginListsOnlyNarrow(t *testing.T) {
	f := newOrg(t)
	withManaged(t, `{"org_policy":{"url":"https://org.example","keys":["`+orgpolicy.EncodeKey(f.pub)+`"]}}`)
	x := strings.Repeat("1", 64)
	doc := `{"plugins":{"allowed_bundles":["` + x + `"]}}`
	f.write(t, doc, time.Now())
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OrgPolicy.Status != orgpolicy.StatusApplied {
		t.Fatalf("status %+v", cfg.OrgPolicy)
	}
	if basis, err := cfg.PluginPolicy().Admit(plugin.Evidence{Digest: x}, nil); err != nil || basis != plugin.BasisOrgPin {
		t.Fatalf("a fresh pinned org's digest did not grant: %q %v", basis, err)
	}
	f.write(t, doc, time.Now().Add(-48*time.Hour))
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OrgPolicy.Status != orgpolicy.StatusFallback {
		t.Fatalf("status %+v", cfg.OrgPolicy)
	}
	pol := cfg.PluginPolicy()
	if _, err := pol.Admit(plugin.Evidence{Digest: x}, nil); err == nil {
		t.Fatal("a stale pinned org policy still granted")
	}
	if _, err := pol.Admit(plugin.Evidence{Digest: strings.Repeat("2", 64), Pin: strings.Repeat("2", 64)}, nil); err == nil {
		t.Fatal("a stale org list stopped narrowing")
	}
	if _, err := pol.Admit(plugin.Evidence{Digest: x, Pin: x}, nil); err != nil {
		t.Fatalf("a pinned bundle the stale list allows: %v", err)
	}
}
