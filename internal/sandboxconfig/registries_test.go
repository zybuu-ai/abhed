package sandboxconfig

import (
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
)

// Where the network is off, package registries get a proxy that denies
// everything until one is opened ("allow": all open); with the network open,
// or registries or toolchains off, none; under a managed network, only where
// the managed file names the setting, and then its own policy is used.
func TestRegistryEgress(t *testing.T) {
	ws := t.TempDir()
	pol := func(cfg config.Config) (bool, *egress.Policy, *egress.Policy) {
		t.Helper()
		p, err := Policy(cfg, ws)
		if err != nil {
			t.Fatal(err)
		}
		return p.AllowNetwork, p.Egress, p.OptionalEgress
	}
	if _, eg, opt := pol(config.Default()); eg != nil || opt == nil || opt.Granted("pypi.org") || opt.Decide(egress.Request{Host: "pypi.org", Port: 443, Tunnel: true}).Allowed() {
		t.Fatal("the default: a closed optional proxy")
	}
	all := config.Default()
	all.Sandbox.ToolchainsRegistries = "allow"
	if _, _, opt := pol(all); opt == nil || !opt.Granted("proxy.golang.org") || !opt.Granted("api.nuget.org") {
		t.Fatal("allow opens them all")
	}
	for _, c := range []func(*config.Config){
		func(c *config.Config) { c.Sandbox.ToolchainsRegistries = "off" },
		func(c *config.Config) { c.Sandbox.Toolchains = "off" },
		func(c *config.Config) { c.Sandbox.AllowNetwork = true },
		func(c *config.Config) {
			c.Managed, c.ManagedKeys, c.Sandbox.AllowNetwork = true, []string{"sandbox.allow_network"}, false
		},
	} {
		cfg := config.Default()
		c(&cfg)
		if _, _, opt := pol(cfg); opt != nil {
			t.Errorf("%+v: a registry proxy", cfg.Sandbox)
		}
	}
	managed := config.Default()
	managed.Managed, managed.ManagedKeys = true, []string{"sandbox.network", "egress", "sandbox.toolchains_registries"}
	managed.Sandbox.Network, managed.Sandbox.ToolchainsRegistries = config.NetworkAllowlist, "allow"
	managed.Egress = egress.Config{Rules: []egress.Rule{{Host: "pypi.org", Decision: "deny"}}}
	_, eg, opt := pol(managed)
	if eg == nil || opt != nil || !eg.Granted("proxy.golang.org") || eg.Decide(egress.Request{Host: "pypi.org", Port: 443, Tunnel: true}).Allowed() {
		t.Fatal("a managed allowlist naming the registries: its own policy, its deny rules winning")
	}
	notNamed := managed
	notNamed.ManagedKeys = []string{"sandbox.network", "egress"}
	notNamed.Egress = egress.Config{}
	if _, eg, _ := pol(notNamed); eg == nil || eg.Granted("proxy.golang.org") || RegistriesOpenable(notNamed) {
		t.Fatal("a managed allowlist that does not name them adds none")
	}
}
