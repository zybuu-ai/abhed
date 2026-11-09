package config

import "testing"

// A trusted workspace never sets prices: a lower price would stretch a
// dollar budget, and a repository arrives with its own.
func TestTrustedWorkspaceNeverSetsPricing(t *testing.T) {
	_, ws := trustHome(t, `{"pricing":{"gpt-4o":{"input":2.5,"output":10}}}`, `{"pricing":{"gpt-4o":{"input":0,"output":0},"cheap":{"input":0,"output":0}}}`)
	cfg, err := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := cfg.Pricing["gpt-4o"]; !ok || p.Input != 2.5 {
		t.Fatalf("the workspace changed a price: %+v", cfg.Pricing)
	}
	if _, ok := cfg.Pricing["cheap"]; ok {
		t.Fatalf("the workspace added a price: %+v", cfg.Pricing)
	}
}

// Under a managed dollar cap, calls are priced by the managed file and the
// built-in list only: the person's own price cannot stretch the cap.
func TestManagedDollarCapIgnoresLowerPrices(t *testing.T) {
	withManaged(t, `{"limits":{"max_budget_usd":1},"pricing":{"local-m":{"input":1,"output":1}}}`)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, home, `{"pricing":{"gpt-4o":{"input":0,"output":0},"local-m":{"input":0,"output":0}}}`)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Pricing["gpt-4o"]; ok {
		t.Fatalf("a user price stayed under a managed cap: %+v", cfg.Pricing)
	}
	if p := cfg.Pricing["local-m"]; p.Input != 1 {
		t.Fatalf("the managed price was not kept: %+v", cfg.Pricing)
	}
	found := false
	for _, k := range cfg.SetAside {
		found = found || k.Key == "pricing.gpt-4o"
	}
	if !found {
		t.Fatalf("the set-aside price was not reported: %+v", cfg.SetAside)
	}
}

// Without a managed cap, the person's own prices stand.
func TestUserPricesWithoutManagedCap(t *testing.T) {
	_, ws := trustHome(t, `{"pricing":{"local-m":{"input":0,"output":0}},"limits":{"max_budget_usd":5}}`, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Pricing["local-m"]; !ok {
		t.Fatalf("pricing %+v", cfg.Pricing)
	}
}
