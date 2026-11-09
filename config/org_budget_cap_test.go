package config

import (
	"slices"
	"testing"
	"time"
)

// A machine's managed caps and the org policy's budget caps both hold: the
// lower applies, so a generic cap in /etc/abhed never lifts what is left of
// an org budget, and the org cannot lift the machine's.
func TestBudgetCapsTakeTheLowerOfManagedAndOrg(t *testing.T) {
	for _, c := range []struct {
		name, managed, org string
		usd                float64
		tokens             int
		fromOrg            []string
	}{
		{"org lower", `{"limits":{"max_budget_usd":50,"max_budget_tokens":1000000}}`,
			`{"limits":{"max_budget_usd":0.75,"max_budget_tokens":700}}`, 0.75, 700,
			[]string{"limits.max_budget_tokens", "limits.max_budget_usd"}},
		{"machine lower", `{"limits":{"max_budget_usd":0.5,"max_budget_tokens":100}}`,
			`{"limits":{"max_budget_usd":0.75,"max_budget_tokens":700}}`, 0.5, 100, nil},
		{"machine none", `{"limits":{"max_budget_usd":0,"max_turns":9}}`,
			`{"limits":{"max_budget_usd":0.75}}`, 0.75, 0, []string{"limits.max_budget_usd"}},
		{"org none", `{"limits":{"max_budget_usd":3}}`, `{"limits":{"max_budget_usd":0}}`, 3, 0, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			withManaged(t, c.managed)
			f := newOrg(t)
			f.write(t, c.org, time.Now())
			cfg, err := Load(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Limits.MaxBudgetUSD != c.usd || cfg.Limits.MaxBudgetTokens != c.tokens {
				t.Fatalf("usd %v tokens %v, want %v %v", cfg.Limits.MaxBudgetUSD, cfg.Limits.MaxBudgetTokens, c.usd, c.tokens)
			}
			for _, k := range c.fromOrg {
				if !slices.Contains(cfg.OrgPolicy.Keys, k) || slices.Contains(cfg.OrgPolicy.Shadowed, k) {
					t.Errorf("%s: keys %v shadowed %v", k, cfg.OrgPolicy.Keys, cfg.OrgPolicy.Shadowed)
				}
			}
		})
	}
}
