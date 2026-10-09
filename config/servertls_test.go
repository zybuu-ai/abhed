package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestServerTLSValidation(t *testing.T) {
	base := func() Config {
		c := Default()
		c.Server.TLS = ServerTLSConfig{CertFile: "c.pem", KeyFile: "k.pem"}
		return c
	}
	if err := base().validateTLS(); err != nil {
		t.Fatalf("a plain pair: %v", err)
	}
	cases := map[string]func(*Config){
		"key without cert":         func(c *Config) { c.Server.TLS.CertFile = "" },
		"cert without key":         func(c *Config) { c.Server.TLS.KeyFile = "" },
		"only a CA":                func(c *Config) { c.Server.TLS = ServerTLSConfig{ClientCAFile: "ca.pem"} },
		"half an additional pair":  func(c *Config) { c.Server.TLS.AdditionalCertificates = []CertificatePair{{CertFile: "b.pem"}} },
		"TLS 1.1":                  func(c *Config) { c.Server.TLS.MinVersion = "1.1" },
		"unknown client auth":      func(c *Config) { c.Server.TLS.ClientAuth = "maybe"; c.Server.TLS.ClientCAFile = "ca.pem" },
		"client auth without CA":   func(c *Config) { c.Server.TLS.ClientAuth = "require" },
		"cert accounts without CA": func(c *Config) { c.Server.TLS.ClientCertAccounts = true },
		"cert accounts, no local": func(c *Config) {
			c.Server.TLS.ClientCertAccounts = true
			c.Server.TLS.ClientCAFile = "ca.pem"
		},
		"negative reload": func(c *Config) { c.Server.TLS.ReloadSeconds = -1 },
	}
	for name, mod := range cases {
		c := base()
		mod(&c)
		if err := c.validateTLS(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := base()
	c.Auth.Mode = "local"
	c.Server.TLS.ClientCAFile, c.Server.TLS.ClientAuth, c.Server.TLS.ClientCertAccounts = "ca.pem", "optional", true
	c.Server.TLS.MinVersion = "1.3"
	if err := c.validateTLS(); err != nil {
		t.Errorf("a full mTLS config: %v", err)
	}
	if (ServerTLSConfig{}).Enabled() {
		t.Error("no settings enable TLS")
	}
	if !(ServerTLSConfig{MinVersion: "1.3"}).Enabled() {
		t.Error("a lone TLS setting must enable TLS, so it is refused rather than ignored")
	}
}

func TestCLILoginValidation(t *testing.T) {
	c := Default()
	c.Auth.CLI.Enabled = true
	if err := c.validateCLILogin(); err == nil || !strings.Contains(err.Error(), "auth.mode local") {
		t.Errorf("cli login without local accounts: %v", err)
	}
	c.Auth.Mode = "proxy"
	if err := c.validateCLILogin(); err == nil {
		t.Error("cli login behind an authenticating proxy was accepted")
	}
	// An edition that serves oidc signs the command line in through it.
	c.Auth.Mode = "oidc"
	if err := c.validateCLILogin(); err != nil {
		t.Errorf("cli login with oidc: %v", err)
	}
	c.Auth.Mode = "local"
	if err := c.validateCLILogin(); err != nil {
		t.Errorf("cli login with local accounts: %v", err)
	}
	c.Auth.CLI.TokenHours = 24*90 + 1
	if err := c.validateCLILogin(); err == nil {
		t.Error("a token lasting more than 90 days was accepted")
	}
}

func TestServerSecretFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	c := Default()
	if got := c.ServerSecretFiles(ws); len(got) != 0 {
		t.Fatalf("no TLS, no cli login: %v", got)
	}
	c.Server.TLS = ServerTLSConfig{CertFile: "c.pem", KeyFile: "keys/k.pem",
		AdditionalCertificates: []CertificatePair{{CertFile: "/etc/x.pem", KeyFile: "/etc/x-key.pem"}}}
	c.Auth.Mode, c.Auth.CLI.Enabled = "local", true
	got := c.ServerSecretFiles(ws)
	key, _ := c.CLIKeyFile(ws)
	for _, want := range []string{filepath.Join(ws, "keys", "k.pem"), "/etc/x-key.pem", key, key + ".revoked"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s missing from %v", want, got)
		}
	}
	if slices.Contains(got, filepath.Join(ws, "c.pem")) {
		t.Error("a certificate is not a secret")
	}
	if !slices.Contains(got, key+CLIEditionStateSuffix) {
		t.Errorf("an edition's sign-in state beside the key is missing from %v", got)
	}
	c.Auth.CLI.KeyFile = "/srv/abhed/cli.key"
	if k, _ := c.CLIKeyFile(ws); k != "/srv/abhed/cli.key" {
		t.Errorf("configured key file = %s", k)
	}
}

// The relay's host key and the snapshot folder are absolute, refused at
// load otherwise, and the configured key is a secret file in clean form.
func TestControlPlanePathsResolved(t *testing.T) {
	for _, mod := range []func(*Config){
		func(c *Config) { c.ControlPlane.SSHHostKey = "keys/host" },
		func(c *Config) { c.ControlPlane.SnapshotDir = "snaps" },
	} {
		c := Default()
		mod(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "not an absolute path") {
			t.Errorf("a relative path was accepted: %v", err)
		}
	}
	t.Setenv("HOME", "/home/u")
	t.Setenv("XDG_STATE_HOME", "/state")
	c := Default()
	c.ControlPlane.Enabled = true
	state, err := ControlPlaneStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if key, err := c.SSHHostKeyFile(); err != nil || key != filepath.Join(state, SSHHostKeyName) {
		t.Errorf("default key %q %v", key, err)
	}
	if snaps, err := c.SnapshotDir(); err != nil || snaps != filepath.Join(state, "snapshots") {
		t.Errorf("default snapshots %q %v", snaps, err)
	}
	c.ControlPlane.SSHHostKey = "/run/keys/../secrets/host"
	if got := c.ServerSecretFiles("/ws"); !slices.Contains(got, "/run/secrets/host") {
		t.Errorf("secret files %q lack the configured key in clean form", got)
	}
}

// The replicas' relay files resolve against the workspace, the same paths
// serve denies to commands, so an edition reads the file that is protected.
func TestResolvedHA(t *testing.T) {
	ws := t.TempDir()
	c := Default()
	c.ControlPlane.HA = HAConfig{Enabled: true, RelayKeyFile: ".abhed/relay.key", RelayCAFile: "/etc/abhed/ca.pem"}
	h := c.ResolvedHA(ws)
	if h.RelayKeyFile != filepath.Join(ws, ".abhed", "relay.key") || h.RelayCAFile != "/etc/abhed/ca.pem" {
		t.Fatalf("resolved %+v", h)
	}
	if c.ControlPlane.HA.RelayKeyFile != ".abhed/relay.key" {
		t.Fatal("resolving changed the configuration")
	}
	got := c.ServerSecretFiles(ws)
	for _, want := range []string{h.RelayKeyFile, h.RelayCAFile} {
		if !slices.Contains(got, want) {
			t.Errorf("%s missing from %v", want, got)
		}
	}
}
