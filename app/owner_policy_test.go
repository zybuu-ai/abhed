package app

import (
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/store"
)

// Only a local-accounts deployment with nothing beside it may move rows to
// accounts; every other mode, or none, unclaims them.
func TestOwnerPolicyFollowsTheAuthMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*config.Config)
		want store.OwnerPolicy
	}{
		{"local", func(c *config.Config) { c.Auth.Mode = "local" }, store.OwnersLocalOnly},
		{"local with a provider", func(c *config.Config) {
			c.Auth.Mode, c.Auth.Provider, c.Auth.ClientID = "local", "google", "id"
		}, store.OwnersUnclaim},
		{"proxy", func(c *config.Config) { c.Auth.Mode = "proxy" }, store.OwnersUnclaim},
		{"oidc", func(c *config.Config) { c.Auth.Mode = "oidc" }, store.OwnersUnclaim},
		{"none", func(c *config.Config) { c.Auth.Mode = "none" }, store.OwnersUnclaim},
		{"unset", func(c *config.Config) { c.Auth.Mode = "" }, store.OwnersUnclaim},
	} {
		cfg := config.Default()
		tc.set(&cfg)
		if got := ownerPolicy(cfg); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
		if got := storeConfig(cfg).Owners; got != tc.want {
			t.Errorf("%s: a single-role start would use %s, want %s", tc.name, got, tc.want)
		}
	}
}
