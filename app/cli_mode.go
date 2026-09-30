package app

import (
	"context"
	"fmt"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/mode", Args: "<name>", Help: "default | accept-edits | plan | auto", Group: "mode", Order: 10, Run: legacy("/mode", slashMode)})
}

// switchMode is /mode. The managed configuration binds it as it binds -mode,
// and bypass is never offered mid-session.
func switchMode(cfg config.Config, pol *policy.Engine, arg string) error {
	switch policy.Mode(arg) {
	case policy.ModeDefault, policy.ModeAcceptEdits, policy.ModePlan, policy.ModeAuto:
	default:
		return fmt.Errorf("unknown mode %q", arg)
	}
	if _, err := cfg.Apply(config.Overrides{Mode: arg}); err != nil {
		return err
	}
	pol.Mode = policy.Mode(arg)
	return nil
}

// slashMode is /mode.
func slashMode(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	if len(fields) < 2 {
		fmt.Printf("  current mode: %s\n", pol.Mode)
		return false
	}
	if err := switchMode(st.appCfg, pol, fields[1]); err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	fmt.Printf("  mode: %s\n", pol.Mode)
	return false
}
