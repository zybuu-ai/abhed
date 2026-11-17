package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/managed"
)

// Who may work on the machine is the organisation's to say: a person's own
// file or a workspace cannot turn guest back on or the sign-in rule off.
func TestUserFileCannotSetClientAuth(t *testing.T) {
	noManaged(t)
	h := t.TempDir()
	t.Setenv("HOME", h)
	_ = os.MkdirAll(filepath.Join(h, ".abhed"), 0o700)
	if err := os.WriteFile(filepath.Join(h, ".abhed", "config.json"),
		[]byte(`{"auth":{"client":{"sign_in":"required","guest":false,"remember_days":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Client.SignIn != "" || !cfg.Auth.Client.GuestAllowed() || cfg.Auth.Client.RememberDays != nil {
		t.Fatalf("a user's file set auth.client: %+v", cfg.Auth.Client)
	}
}

func TestManagedFileSetsClientAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"auth":{"client":{"sign_in":"server","server":"https://abhed.corp/","guest":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	withManagedFile(t, path)
	cfg, err := LoadManaged()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Client.SignIn != SignInServer || cfg.SignInServer() != "https://abhed.corp" || cfg.Auth.Client.GuestAllowed() {
		t.Fatalf("auth.client = %+v", cfg.Auth.Client)
	}
}

func TestValidateClientAuth(t *testing.T) {
	neg, big := -1, 4000
	for name, c := range map[string]ClientAuthConfig{
		"unknown sign_in":        {SignIn: "always"},
		"server with no server":  {SignIn: SignInServer},
		"server not a URL":       {Server: "abhed.corp"},
		"negative remember_days": {RememberDays: &neg},
		"huge remember_days":     {RememberDays: &big},
	} {
		cfg := Default()
		cfg.Auth.Client = c
		if err := cfg.validateClientAuth(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cfg := Default()
	cfg.Auth.Client = ClientAuthConfig{SignIn: SignInServer}
	cfg.Org.URL = "https://org.example"
	if err := cfg.validateClientAuth(); err != nil || cfg.SignInServer() != "https://org.example" {
		t.Errorf("sign_in server falls back to the org policy's server: %v %q", err, cfg.SignInServer())
	}
}

// The org policy may require sign-in, turn guest off and bound a remembered
// sign-in; never the reverse, and never name a server.
func TestOrgPolicyOnlyTightensClientAuth(t *testing.T) {
	noManaged(t)
	f := newOrg(t)
	f.write(t, `{"auth":{"client":{"sign_in":"required","guest":false,"remember_days":7}}}`, time.Now())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Client.SignIn != SignInRequired || cfg.Auth.Client.GuestAllowed() || cfg.Auth.Client.RememberDays == nil || *cfg.Auth.Client.RememberDays != 7 {
		t.Fatalf("the org policy's tightening did not apply: %+v", cfg.Auth.Client)
	}
	f.write(t, `{"auth":{"client":{"sign_in":"","guest":true,"server":"https://evil.example"}}}`, time.Now())
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Client.Server != "" || cfg.Auth.Client.Guest != nil {
		t.Fatalf("the org policy loosened or named a server: %+v", cfg.Auth.Client)
	}
}

func withManagedFile(t *testing.T, path string) {
	t.Helper()
	old := managed.ConfigFile
	managed.ConfigFile = path
	t.Cleanup(func() { managed.ConfigFile = old })
}
