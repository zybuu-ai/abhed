package config

import (
	"strings"
	"testing"
)

// sandbox.tier takes process, container, vm and fence, never one weaker than
// the minimum in force.
func TestSandboxTierValues(t *testing.T) {
	for tier, min := range map[string]string{"process": "process", "container": "container", "vm": "container", "fence": "process", "": "vm"} {
		c := Default()
		c.Sandbox.Tier, c.Sandbox.MinTier = tier, min
		if err := c.Validate(); err != nil {
			t.Errorf("tier %q, min %q: %v", tier, min, err)
		}
	}
	for tier, min := range map[string]string{"none": "none", "outer": "process", "process": "container", "container": "vm"} {
		c := Default()
		c.Sandbox.Tier, c.Sandbox.MinTier = tier, min
		if err := c.Validate(); err == nil {
			t.Errorf("tier %q with min %q was taken", tier, min)
		}
	}
	c := Default()
	c.Sandbox.Tier, c.Sandbox.MinTier = "process", "container"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "weaker than sandbox.min_tier container") {
		t.Fatalf("got %v", err)
	}
}

// A managed min_tier binds a person's chosen tier: loaded together, a
// process tier under a managed container minimum is refused.
func TestManagedMinTierBindsTheChosenTier(t *testing.T) {
	withManaged(t, `{"sandbox": {"min_tier": "container"}}`)
	ws := t.TempDir()
	writeUserFile(t, `{"sandbox": {"tier": "process"}}`)
	if _, err := Load(ws); err == nil || !strings.Contains(err.Error(), "weaker than sandbox.min_tier") {
		t.Fatalf("got %v", err)
	}
	writeUserFile(t, `{"sandbox": {"tier": "vm"}}`)
	cfg, err := Load(ws)
	if err != nil || cfg.Sandbox.Tier != "vm" {
		t.Fatalf("vm: %v %q", err, cfg.Sandbox.Tier)
	}
}
