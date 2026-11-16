package config

import (
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
