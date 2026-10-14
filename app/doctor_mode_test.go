package app

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// doctor and the serve banner say when a managed file disables bypass mode.
func TestModeLabelSaysBypassIsDisabled(t *testing.T) {
	var cfg config.Config
	cfg.Permissions.Mode = "bypass"
	if got := modeLabel(cfg); got != "bypass" {
		t.Errorf("unmanaged: %q", got)
	}
	cfg.Managed = true
	if got := modeLabel(cfg); !strings.Contains(got, "bypass disabled by "+managed.ConfigFile) {
		t.Errorf("managed: %q", got)
	}
	cfg.Permissions.Mode = "auto"
	if got := modeLabel(cfg); got != "auto" || bypassDisabled(cfg) != "" {
		t.Errorf("managed auto: %q", got)
	}
}
