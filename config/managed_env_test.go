package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// envAside lists the settings the environment was refused, by variable.
func envAside(cfg Config) map[string]string {
	out := map[string]string{}
	for _, k := range cfg.SetAside {
		if k.Layer == LayerEnv {
			out[k.File] = k.Key
		}
	}
	return out
}

// Every ABHED_* variable that changes a setting yields to the managed file
// when it sets that setting, and the warning names the setting.
func TestEnvYieldsToTheManagedFile(t *testing.T) {
	withManaged(t, `{
	  "model": {"default": "corp", "providers": {"corp": {"type": "openai-compatible",
	    "base_url": "https://llm.corp.example/v1", "model": "corp-model", "api_key": "corp-key", "context_window": 8192}}},
	  "storage": {"driver": "postgres", "dsn": "postgres://corp/db", "migrate_dsn": "postgres://corp/owner"},
	  "auth": {"mode": "local", "users_file": "/etc/abhed/users.json"}}`)
	var warned strings.Builder
	warnOut = &warned
	t.Cleanup(func() { warnOut = os.Stderr })
	env := map[string]string{
		"ABHED_BASE_URL":             "http://attacker.example/v1",
		"ABHED_MODEL":                "other-model",
		"ABHED_API_KEY":              "other-key",
		"ABHED_DATABASE_URL":         "postgres://other/db",
		"ABHED_MIGRATE_DATABASE_URL": "postgres://other/owner",
		"ABHED_USERS_FILE":           "/tmp/users.json",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Model.Providers["corp"]
	if p.BaseURL != "https://llm.corp.example/v1" || p.Model != "corp-model" || p.APIKey != "corp-key" {
		t.Errorf("the environment changed the managed provider: %+v", p)
	}
	if cfg.Storage.DSN != "postgres://corp/db" || cfg.Storage.MigrateDSN != "postgres://corp/owner" ||
		cfg.Auth.UsersFile != "/etc/abhed/users.json" {
		t.Errorf("the environment changed managed deployment settings: %+v %+v", cfg.Storage, cfg.Auth)
	}
	want := map[string]string{
		"ABHED_BASE_URL":             "model.providers.corp.base_url",
		"ABHED_MODEL":                "model.providers.corp.model",
		"ABHED_API_KEY":              "model.providers.corp.api_key",
		"ABHED_DATABASE_URL":         "storage.dsn",
		"ABHED_MIGRATE_DATABASE_URL": "storage.migrate_dsn",
		"ABHED_USERS_FILE":           "auth.users_file",
	}
	got := envAside(cfg)
	for v, key := range want {
		if got[v] != key {
			t.Errorf("%s: set aside as %q, want %q", v, got[v], key)
		}
		if !strings.Contains(warned.String(), v+" sets "+key+", which is ignored") {
			t.Errorf("no warning names %s for %s:\n%s", key, v, warned.String())
		}
	}
	for _, v := range env {
		if strings.Contains(warned.String(), v) {
			t.Errorf("the warning repeats the value %q", v)
		}
	}
}

// A managed file that sets other keys leaves the environment its say over the rest.
func TestEnvStillSetsWhatTheManagedFileLeaves(t *testing.T) {
	withManaged(t, `{"permissions": {"mode": "default"}, "storage": {"driver": "memory"}}`)
	t.Setenv("ABHED_BASE_URL", "http://from-env:9000/v1")
	t.Setenv("ABHED_MODEL", "env-model")
	t.Setenv("ABHED_USERS_FILE", "/srv/users.json")
	t.Setenv("ABHED_DATABASE_URL", "postgres://env/db")
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, _ := cfg.Provider()
	if p.BaseURL != "http://from-env:9000/v1" || p.Model != "env-model" || cfg.Auth.UsersFile != "/srv/users.json" {
		t.Errorf("the environment no longer applies: %+v %q", p, cfg.Auth.UsersFile)
	}
	// The managed file pins the memory driver: the DSN is kept, the driver is not switched.
	if cfg.Storage.DSN != "postgres://env/db" || cfg.Storage.Driver != "memory" {
		t.Errorf("storage %+v", cfg.Storage)
	}
	if got := envAside(cfg); !slices.Equal([]string{got["ABHED_DATABASE_URL"]}, []string{"storage.driver"}) || len(got) != 1 {
		t.Errorf("set aside %v, want only storage.driver", got)
	}
}

// A managed file that names the default provider's entry binds every field
// of it, the ones it leaves out included: the entry is one setting.
func TestEnvYieldsToAManagedProviderEntry(t *testing.T) {
	withManaged(t, `{"model": {"providers": {"local": {"type": "openai-compatible",
	  "base_url": "http://127.0.0.1:8000/v1", "model": "pinned", "context_window": 8192}}}}`)
	t.Setenv("ABHED_API_KEY", "user-key")
	t.Setenv("ABHED_BASE_URL", "http://elsewhere:9000/v1")
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Default != "local" {
		t.Skipf("the built-in default is %q", cfg.Model.Default)
	}
	p := cfg.Model.Providers["local"]
	if p.APIKey != "" || p.BaseURL != "http://127.0.0.1:8000/v1" {
		t.Errorf("the environment changed the managed entry: %+v", p)
	}
}
