package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
)

// A config naming a feature this binary lacks is refused by name, not
// silently served without it. The message has to carry the key, the tier and
// what to do, because the person reading it has a config that is valid — it
// is the binary that is the wrong one.
func TestEditionRefusesKeysItCannotHonour(t *testing.T) {
	community := newApp()

	for _, tc := range []struct {
		name   string
		adjust func(*config.Config)
		key    string
		tier   string
	}{
		{"oidc", func(c *config.Config) { c.Auth.Mode = "oidc" }, `auth.mode "oidc"`, "Team"},
		{"local plus provider", func(c *config.Config) {
			c.Auth.Mode = "local"
			c.Auth.Provider = "google"
			c.Auth.ClientID = "id"
		}, `auth.provider "google"`, "Team"},
		{"telemetry", func(c *config.Config) { c.Telemetry.Enabled = true }, "telemetry.enabled", "Enterprise"},
		{"schedules", func(c *config.Config) {
			c.Schedules = []config.ScheduleConfig{{Name: "n", Cron: "@daily", Prompt: "p"}}
		}, "schedules", "Team"},
		{"ha", func(c *config.Config) { c.ControlPlane.HA.Enabled = true }, "control_plane.ha.enabled", "Enterprise"},
		{"interceptors", func(c *config.Config) {
			c.ControlPlane.Interceptors = []config.InterceptorConfig{{Name: "opa", Phase: "validate", URL: "https://opa"}}
		}, "control_plane.interceptors", "Enterprise"},
		{"workload", func(c *config.Config) { c.Auth.Workload.Enabled = true }, "auth.workload.enabled", "Enterprise"},
		{"kubernetes", func(c *config.Config) {
			c.ControlPlane.Kubernetes = json.RawMessage(`{"namespace":"abhed"}`)
		}, "control_plane.kubernetes", "Team"},
	} {
		cfg := config.Default()
		tc.adjust(&cfg)
		err := community.checkEdition(cfg)
		if err == nil {
			t.Errorf("%s: a Community binary accepted the key", tc.name)
			continue
		}
		msg := err.Error()
		for _, want := range []string{tc.key, tc.tier + " feature", "Community Edition", "Remove the key"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: message %q does not say %q", tc.name, msg, want)
			}
		}
	}

	// The same keys pass once the features are registered. The builder is a
	// stand-in: what is tested is the registry, not the provider behind it.
	full := newApp(
		WithAuthMode("oidc", func(context.Context, config.Config, string, auth.UserStore) (auth.Provider, auth.TokenVerifier, error) {
			return nil, nil, nil
		}),
		WithFeature("schedules"), WithFeature("telemetry"), WithFeature("ha"), WithFeature("interceptors"), WithFeature("workload"), WithFeature("kubernetes"))
	cfg := config.Default()
	cfg.ControlPlane.HA.Enabled = true
	cfg.ControlPlane.Interceptors = []config.InterceptorConfig{{Name: "opa", Phase: "validate", URL: "https://opa"}}
	cfg.Auth.Mode = "oidc"
	cfg.Auth.Workload.Enabled = true
	cfg.ControlPlane.Kubernetes = json.RawMessage(`{"namespace":"abhed"}`)
	cfg.Telemetry.Enabled = true
	cfg.Schedules = []config.ScheduleConfig{{Name: "n", Cron: "@daily", Prompt: "p"}}
	if err := full.checkEdition(cfg); err != nil {
		t.Errorf("a binary with every feature refused a config: %v", err)
	}

	// Keys that are Community in every edition are never refused.
	cfg = config.Default()
	cfg.Auth.Mode = "local"
	cfg.Storage.Tenant = "acme"
	if err := community.checkEdition(cfg); err != nil {
		t.Errorf("Community keys refused: %v", err)
	}
}

// Local accounts need no registration and proxy mode nothing to build; a
// builder is only demanded for a mode that has one.
func TestAuthModesFollowTheConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	if got := authModes(cfg); len(got) != 1 || got[0] != "local" {
		t.Errorf("local: %v", got)
	}
	cfg.Auth.Provider, cfg.Auth.ClientID = "google", "id"
	if got := authModes(cfg); len(got) != 2 || got[1] != "oidc" {
		t.Errorf("local + provider: %v", got)
	}
	cfg.Auth.Mode = "none"
	if got := authModes(cfg); len(got) != 0 {
		t.Errorf("none: %v", got)
	}
}
