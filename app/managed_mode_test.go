package app

import (
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
)

func loadWith(t *testing.T, managedBody string) config.Config {
	t.Helper()
	managedConfig(t, managedBody)
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// /mode is bound as -mode is: under a pinned mode only that mode or plan.
func TestModeCommandHonoursAManagedMode(t *testing.T) {
	cfg := loadWith(t, `{"permissions": {"mode": "default"}}`)
	pol := policy.New(policy.ModeDefault)
	for _, m := range []string{"auto", "accept-edits"} {
		var me *config.ManagedError
		if err := switchMode(cfg, pol, m); !errors.As(err, &me) || pol.Mode != policy.ModeDefault {
			t.Errorf("/mode %s: %v, mode now %s", m, err, pol.Mode)
		}
	}
	for _, m := range []string{"plan", "default"} {
		if err := switchMode(cfg, pol, m); err != nil || string(pol.Mode) != m {
			t.Errorf("/mode %s: %v, mode now %s", m, err, pol.Mode)
		}
	}
	if err := switchMode(cfg, pol, "bypass"); err == nil || pol.Mode != policy.ModeDefault {
		t.Errorf("/mode bypass: %v", err)
	}

	cfg = loadWith(t, "")
	if err := switchMode(cfg, pol, "auto"); err != nil || pol.Mode != policy.ModeAuto {
		t.Errorf("without a managed file /mode auto: %v", err)
	}
}

// resolve's default of auto yields to a pinned managed mode; an explicit flag does not.
func TestResolveModeYieldsToAManagedMode(t *testing.T) {
	parse := func(args string) (*flag.FlagSet, string) {
		fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
		mode := fs.String("mode", "auto", "")
		_ = fs.Parse(strings.Fields(args))
		return fs, *mode
	}
	cfg := loadWith(t, `{"permissions": {"mode": "default"}}`)
	for args, want := range map[string]string{"": "", "-mode auto": "auto", "-mode plan": "plan"} {
		fs, mode := parse(args)
		if got := resolveMode(cfg, fs, mode); got != want {
			t.Errorf("%q: mode %q, want %q", args, got, want)
		}
	}
	cfg = loadWith(t, "")
	if fs, mode := parse(""); resolveMode(cfg, fs, mode) != "auto" {
		t.Error("without a managed file the default is no longer auto")
	}
}
