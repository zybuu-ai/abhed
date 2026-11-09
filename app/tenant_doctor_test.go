package app

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// Doctor warns, naming both settings and the "tenant" claim fallback, when sign-in is
// through an identity provider, storage.tenant is not default and auth.tenant_claim is
// unset; not when a claim or a named provider is set, or the tenant is default.
func TestDoctorWarnsOfATenantNoClaimCarries(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode, cfg.Storage.Tenant = "oidc", "corp"
	w := tenantClaimWarning(cfg)
	for _, want := range []string{"storage.tenant", "auth.tenant_claim", `"tenant"`} {
		if !strings.Contains(w, want) {
			t.Errorf("the warning lacks %s: %s", want, w)
		}
	}
	for name, mut := range map[string]func(*config.Config){
		"claim":    func(c *config.Config) { c.Auth.TenantClaim = "org_id" },
		"provider": func(c *config.Config) { c.Auth.Provider = "microsoft" },
		"default":  func(c *config.Config) { c.Storage.Tenant = "default" },
		"local":    func(c *config.Config) { c.Auth.Mode = "local" },
	} {
		c := cfg
		mut(&c)
		if w := tenantClaimWarning(c); w != "" {
			t.Errorf("%s: warned: %s", name, w)
		}
	}
}
