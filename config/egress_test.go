package config

import (
	"strings"
	"testing"
)

// The egress allowlist and its rules are the managed file's alone: the
// user's file cannot turn it on or name destinations, and the managed
// file's values hold.
func TestEgressIsManagedOnly(t *testing.T) {
	for _, k := range []string{"sandbox.network", "egress.rules", "egress.default", "egress.mode", "egress.record_paths", "egress.idle_seconds"} {
		if !ManagedOnly(k) {
			t.Errorf("%s is not managed only", k)
		}
	}
	user := `{"sandbox":{"network":"allowlist"},"egress":{"default":"allow","mode":"audit","record_paths":false,"idle_seconds":99999,"rules":[{"host":"evil.example","decision":"allow"}]}}`
	_, ws := trustHome(t, user, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Network != "" || len(cfg.Egress.Rules) != 0 || cfg.Egress.Default != "" || cfg.Egress.Mode != "" ||
		cfg.Egress.RecordPaths != nil || cfg.Egress.IdleSeconds != 0 {
		t.Fatalf("the user's file set egress: %+v %+v", cfg.Sandbox, cfg.Egress)
	}
	if len(cfg.SetAside) < 6 {
		t.Fatalf("set aside: %v", cfg.SetAside)
	}

	withManaged(t, `{"sandbox":{"network":"allowlist"},"egress":{"record_paths":false,"idle_seconds":60,"rules":[{"host":"*.example.com","ports":[443],"decision":"allow"}]}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Network != NetworkAllowlist || len(cfg.Egress.Rules) != 1 || cfg.Egress.Rules[0].Host != "*.example.com" {
		t.Fatalf("the managed egress was not kept: %+v %+v", cfg.Sandbox, cfg.Egress)
	}
	if cfg.Egress.RecordPaths == nil || *cfg.Egress.RecordPaths || cfg.Egress.IdleSeconds != 60 {
		t.Fatalf("the managed record_paths and idle_seconds were not kept: %+v", cfg.Egress)
	}
}

func TestEgressValidation(t *testing.T) {
	cfg := Default()
	cfg.Sandbox.Network = "partial"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "sandbox.network") {
		t.Fatalf("sandbox.network: %v", err)
	}
	cfg = Default()
	cfg.Egress.IdleSeconds = -5
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "egress.idle_seconds") {
		t.Fatalf("egress.idle_seconds: %v", err)
	}
	cfg = Default()
	cfg.Egress.Mode = "loud"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "egress.mode") {
		t.Fatalf("egress.mode: %v", err)
	}
}
