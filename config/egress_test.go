package config

import (
	"strings"
	"testing"
)

// The egress allowlist and its rules are the managed file's alone: the
// user's file cannot turn it on or name destinations, and the managed
// file's values hold.
func TestEgressIsManagedOnly(t *testing.T) {
	for _, k := range []string{"sandbox.network", "egress.rules", "egress.default", "egress.mode", "egress.record_paths", "egress.idle_seconds", "egress.inspect_tls"} {
		if !ManagedOnly(k) {
			t.Errorf("%s is not managed only", k)
		}
	}
	user := `{"sandbox":{"network":"allowlist"},"egress":{"default":"allow","mode":"audit","record_paths":false,"idle_seconds":99999,"inspect_tls":true,"rules":[{"host":"evil.example","decision":"allow"}]}}`
	_, ws := trustHome(t, user, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Network != "" || len(cfg.Egress.Rules) != 0 || cfg.Egress.Default != "" || cfg.Egress.Mode != "" ||
		cfg.Egress.RecordPaths != nil || cfg.Egress.IdleSeconds != 0 || cfg.Egress.InspectTLS {
		t.Fatalf("the user's file set egress: %+v %+v", cfg.Sandbox, cfg.Egress)
	}
	if len(cfg.SetAside) < 7 {
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

// A set-aside egress setting is recorded with the value it was given, from
// the user's file or a trusted workspace's; an untrusted workspace's is
// recorded as ignored, with the same value.
func TestRefusedEgressValuesAreRecorded(t *testing.T) {
	file := `{"sandbox":{"network":"allowlist"},"egress":{"default":"allow","rules":[{"host":"evil.example","decision":"allow"}]}}`
	want := map[string]string{"sandbox.network": `"allowlist"`, "egress.default": `"allow"`, "egress.rules": "evil.example"}
	check := func(label string, cfg Config, decision string) {
		t.Helper()
		got := map[string]string{}
		for _, a := range cfg.Attempts() {
			if _, ok := want[a.Key]; ok && a.Decision == decision {
				got[a.Key] = a.Value
			}
		}
		for k, v := range want {
			if !strings.Contains(got[k], v) {
				t.Errorf("%s: %s recorded as %q, want it to hold %s (%v)", label, k, got[k], v, cfg.Attempts())
			}
		}
	}
	_, ws := trustHome(t, file, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	check("user", cfg, "set_aside")
	_, ws = trustHome(t, "", file)
	if cfg, err = LoadWith(ws, LoadOptions{Quiet: true, Trust: TrustGranted}); err != nil {
		t.Fatal(err)
	}
	check("trusted workspace", cfg, "set_aside")
	if cfg, err = LoadWith(ws, LoadOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	check("untrusted workspace", cfg, "ignored_untrusted")
}

// The person's own file may narrow allow_network to the egress allowlist;
// over no network, from a trusted workspace or against a managed value it
// is set aside, so it never widens.
func TestUserFileMayNarrowToTheAllowlist(t *testing.T) {
	own := `{"sandbox":{"allow_network":true,"network":"allowlist"},"egress":{"rules":[{"host":"proxy.golang.org","decision":"allow"}]}}`
	_, ws := trustHome(t, own, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Network != NetworkAllowlist || len(cfg.Egress.Rules) != 1 || len(cfg.SetAside) != 0 {
		t.Fatalf("the user's allowlist was set aside: %+v", cfg.SetAside)
	}
	_, ws = trustHome(t, `{"sandbox":{"network":"allowlist"}}`, "")
	if cfg, err = LoadWith(ws, LoadOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Network != "" {
		t.Fatal("an allowlist over no network was kept")
	}
	_, ws = trustHome(t, `{"sandbox":{"allow_network":true}}`, `{"sandbox":{"network":"allowlist"}}`)
	if cfg, err = LoadWith(ws, LoadOptions{Quiet: true, Trust: TrustGranted}); err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Network != "" {
		t.Fatal("a workspace's allowlist was kept")
	}
	withManaged(t, `{"egress":{"default":"deny"}}`)
	_, ws = trustHome(t, own, "")
	if cfg, err = LoadWith(ws, LoadOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Network != "" || len(cfg.Egress.Rules) != 0 {
		t.Fatalf("over a managed egress setting: network %q rules %v", cfg.Sandbox.Network, cfg.Egress.Rules)
	}
	withManaged(t, `{"sandbox":{"allow_network":false}}`)
	_, ws = trustHome(t, own, "")
	if cfg, err = LoadWith(ws, LoadOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Network != "" || cfg.Sandbox.AllowNetwork {
		t.Fatalf("over a managed allow_network false: network %q allow %v", cfg.Sandbox.Network, cfg.Sandbox.AllowNetwork)
	}
}
