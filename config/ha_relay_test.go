package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The settings that decide whom serve trusts, or what it runs, come only from
// the managed file; the rest of the relay settings from any server file.
func TestEditionSecuritySettingsAreManagedOnly(t *testing.T) {
	for _, k := range []string{"auth.bound_claims", "auth.bound_claims.hd", "control_plane.ha.relay_peer", "control_plane.ha.relay_key_command"} {
		if !ManagedOnly(k) {
			t.Errorf("%s is not managed only", k)
		}
	}
	if ManagedOnly("control_plane.ha.relay_cert_file") || ManagedOnly("auth.bound") {
		t.Error("a setting that is not managed only was taken for one")
	}
	user := `{"auth":{"bound_claims":{"hd":["evil.example"]}},"control_plane":{"ha":{"relay_peer":"evil","relay_key_command":["/tmp/evil"],` +
		`"relay_cert_file":"r.pem","relay_cert_key_file":"r.key","relay_key_vault":"ha-relay#key"}}}`
	_, ws := trustHome(t, user, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.ControlPlane.HA
	if cfg.Auth.BoundClaims != nil || h.RelayPeer != "" || h.RelayKeyCommand != nil {
		t.Fatalf("the user's file set managed-only settings: %+v %+v", cfg.Auth.BoundClaims, h)
	}
	if h.RelayCertFile != "r.pem" || h.RelayCertKeyFile != "r.key" || h.RelayKeyVault != "ha-relay#key" {
		t.Fatalf("the user's own relay settings were dropped: %+v", h)
	}
	var aside []string
	for _, k := range cfg.SetAside {
		aside = append(aside, k.Key)
	}
	for _, want := range []string{"auth.bound_claims.hd", "control_plane.ha.relay_peer", "control_plane.ha.relay_key_command"} {
		if !slices.Contains(aside, want) {
			t.Errorf("%s was not named as set aside: %v", want, aside)
		}
	}

	withManaged(t, `{"auth":{"bound_claims":{"repository_owner":["acme"]}},"control_plane":{"ha":{"relay_peer":"abhed-replica",`+
		`"relay_cert_file":"/etc/abhed/r.pem","relay_cert_key_file":"/etc/abhed/r.key","relay_key_command":["/usr/local/bin/kms-unwrap","-k","relay"]}}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	h = cfg.ControlPlane.HA
	if got := cfg.Auth.BoundClaims["repository_owner"]; !slices.Equal(got, []string{"acme"}) || h.RelayPeer != "abhed-replica" ||
		!slices.Equal(h.RelayKeyCommand, []string{"/usr/local/bin/kms-unwrap", "-k", "relay"}) {
		t.Fatalf("the managed settings were not kept: %+v %+v", cfg.Auth.BoundClaims, h)
	}
}

func TestEditionSettingsAreChecked(t *testing.T) {
	for name, managed := range map[string]string{
		"claim without values": `{"auth":{"bound_claims":{"hd":[]}}}`,
		"claim admitting all":  `{"auth":{"bound_claims":{"sub":["*"]}}}`,
		"cert without key":     `{"control_plane":{"ha":{"relay_cert_file":"/r.pem"}}}`,
		"peer without cert":    `{"control_plane":{"ha":{"relay_peer":"x"}}}`,
		"vault without field":  `{"control_plane":{"ha":{"relay_key_vault":"ha-relay"}}}`,
		"relative command":     `{"control_plane":{"ha":{"relay_key_command":["kms-unwrap"]}}}`,
	} {
		withManaged(t, managed)
		if _, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The variables an edition read before stay a fallback for what the
// configuration leaves unset, and never override it.
func TestEditionSettingsFallBackToTheEnvironment(t *testing.T) {
	t.Setenv(EnvOIDCBoundClaims, `{"hd":["acme.com"]}`)
	t.Setenv(EnvHARelayCertFile, "env.pem")
	t.Setenv(EnvHARelayCertKeyFile, "/etc/abhed/env.key")
	t.Setenv(EnvHARelayPeer, "env-peer")
	t.Setenv(EnvHARelayKeyCommand, `["/usr/local/bin/kms","unwrap"]`)
	var c Config
	c.ControlPlane.HA.Enabled = true
	bound, src, err := c.OIDCBoundClaims()
	if err != nil || src != EnvOIDCBoundClaims || !slices.Equal(bound["hd"], []string{"acme.com"}) {
		t.Fatalf("bound claims from the environment = %v %q %v", bound, src, err)
	}
	ws := t.TempDir()
	h := c.ResolvedHA(ws)
	if h.RelayCertFile != filepath.Join(ws, "env.pem") || h.RelayCertKeyFile != "/etc/abhed/env.key" || h.RelayPeer != "env-peer" ||
		!slices.Equal(h.RelayKeyCommand, []string{"/usr/local/bin/kms", "unwrap"}) {
		t.Fatalf("relay settings from the environment = %+v", h)
	}
	if !slices.Contains(c.ServerSecretFiles(ws), "/etc/abhed/env.key") {
		t.Fatalf("the relay's client key is not protected: %v", c.ServerSecretFiles(ws))
	}

	c.Auth.BoundClaims = map[string][]string{"hd": {"corp.example"}}
	c.ControlPlane.HA.RelayPeer = "config-peer"
	c.ControlPlane.HA.RelayKeyCommand = []string{"/opt/kms"}
	if bound, src, _ := c.OIDCBoundClaims(); src != "auth.bound_claims" || bound["hd"][0] != "corp.example" {
		t.Fatalf("the environment overrode auth.bound_claims: %v %q", bound, src)
	}
	if h := c.ResolvedHA(ws); h.RelayPeer != "config-peer" || !slices.Equal(h.RelayKeyCommand, []string{"/opt/kms"}) {
		t.Fatalf("the environment overrode the configuration: %+v", h)
	}

	c.Auth.BoundClaims = nil
	for _, bad := range []string{`{"hd":["*"]}`, `["hd"]`, `{"hd":["a"],"hd":["b"]}`} {
		t.Setenv(EnvOIDCBoundClaims, bad)
		if _, _, err := c.OIDCBoundClaims(); err == nil || !strings.Contains(err.Error(), EnvOIDCBoundClaims) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}
