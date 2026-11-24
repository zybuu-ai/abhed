package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An org policy that does not pin its keys may price a model at 0, and no
// more: the 0 is kept and marked to hold only on this machine; any other
// price is set aside, so an unpinned org can never lower a hosted model's
// price to stretch a dollar budget.
func TestUnpinnedOrgPricesOnlyAtZero(t *testing.T) {
	noManaged(t)
	f := newOrg(t)
	f.write(t, `{"limits":{"max_budget_usd":5},"pricing":{"gemma4:26b":{"input":0,"output":0}}}`, time.Now())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := cfg.Pricing["gemma4:26b"]; !ok || p.Input != 0 || p.Output != 0 || !cfg.PricingLoopbackOnly["gemma4:26b"] {
		t.Fatalf("pricing %v loopback-only %v", cfg.Pricing, cfg.PricingLoopbackOnly)
	}

	f.write(t, `{"limits":{"max_budget_usd":5},"pricing":{"gemma4:26b":{"input":0,"output":0},"gpt-4o":{"input":0.01,"output":0}}}`, time.Now())
	cfg, err = Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Pricing) != 0 {
		t.Fatalf("an unpinned org's non-zero price was taken: %v", cfg.Pricing)
	}
	set := false
	for _, s := range cfg.SetAside {
		set = set || s.Key == "pricing" && s.Layer == LayerOrg
	}
	if !set {
		t.Fatalf("the pricing was not set aside: %+v", cfg.SetAside)
	}
}

// A spent budget from the org policy holds; a policy cannot lift one the
// machine's file sets.
func TestBudgetSpentOnlyTightens(t *testing.T) {
	noManaged(t)
	f := newOrg(t)
	f.write(t, `{"limits":{"budget_spent":true}}`, time.Now())
	cfg, err := Load(t.TempDir())
	if err != nil || !cfg.Limits.BudgetSpent {
		t.Fatalf("org spent: %v %v", err, cfg.Limits.BudgetSpent)
	}
	withManaged(t, `{"limits":{"budget_spent":true}}`)
	f.write(t, `{"limits":{"budget_spent":false}}`, time.Now())
	if cfg, err = Load(t.TempDir()); err != nil || !cfg.Limits.BudgetSpent {
		t.Fatalf("lifted by the org: %v %v", err, cfg.Limits.BudgetSpent)
	}
}

// An unpinned org's 0 never replaces a price a lower layer set without a
// managed dollar cap: the person's price stands. Under one, which sets the
// person's prices aside, the person's price does not count for the decision
// either: it is set aside, said once, and the org's 0 stays, held to this
// machine's local servers as for a model nobody priced.
func TestUnpinnedOrgZeroNeverReplacesAPrice(t *testing.T) {
	noManaged(t)
	f := newOrg(t)
	f.write(t, `{"pricing":{"house-model":{"input":0,"output":0}}}`, time.Now())
	ws := t.TempDir()
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(`{"pricing":{"house-model":{"input":3,"output":15}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(ws)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Pricing["house-model"]; p.Input != 3 || cfg.PricingLoopbackOnly["house-model"] {
		t.Fatalf("no cap: %+v loopback-only %v", p, cfg.PricingLoopbackOnly)
	}
	f.write(t, `{"limits":{"max_budget_usd":5},"pricing":{"house-model":{"input":0,"output":0}}}`, time.Now())
	if cfg, err = Load(ws); err != nil {
		t.Fatal(err)
	}
	if p, ok := cfg.Pricing["house-model"]; !ok || p.Input != 0 || !cfg.PricingLoopbackOnly["house-model"] {
		t.Fatalf("under the cap the person's set-aside price dropped the org's 0, or freed it from this machine: %v %v", cfg.Pricing, cfg.PricingLoopbackOnly)
	}
	var org, mine int
	for _, a := range cfg.SetAside {
		if a.Key == "pricing.house-model" {
			if a.Layer == LayerOrg {
				org++
			} else {
				mine++
			}
		}
	}
	if org != 0 || mine != 1 {
		t.Fatalf("want only the person's price set aside, once: %+v", cfg.SetAside)
	}
}

// A provider a workspace's file defines is noted as the workspace's, unless
// the managed layer sets it; the person's own are not.
func TestWorkspaceProvidersAreNoted(t *testing.T) {
	_, ws := trustHome(t, `{"model":{"providers":{"mine":{"type":"ollama","model":"house-model"}}}}`,
		`{"model":{"providers":{"gw":{"type":"vllm","base_url":"http://127.0.0.1:4000/v1","model":"house-model"}}}}`)
	t.Setenv(TrustEnv, "1")
	withManaged(t, `{"limits":{"max_budget_usd":1}}`)
	cfg, err := Load(ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Model.Providers["gw"]; !ok {
		t.Skip("the workspace's provider did not load (workspace not trusted here)")
	}
	if !cfg.WorkspaceProviders["gw"] || cfg.WorkspaceProviders["mine"] {
		t.Fatalf("workspace providers %v", cfg.WorkspaceProviders)
	}
}
