package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
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

// ModeController is the one way the session's permission mode changes: the
// startup flag, /mode, Shift-Tab and the answer to a proposed plan all go
// through it, and it holds the rules they share.
type ModeController interface {
	// Offered is the Shift-Tab cycle, in order: default, accept-edits and
	// plan, less what the managed configuration leaves out or refuses. It
	// never holds auto or bypass.
	Offered() []policy.Mode
	// Cycle moves to the mode after the current one in Offered, or to the
	// first when the current one is not in it, and returns the new mode.
	Cycle() (policy.Mode, error)
	// Set changes to mode. via is how: agent.ViaFlag, ViaSlash, ViaShiftTab
	// or ViaPlanExit. Bypass comes only from the flag, Shift-Tab only reaches
	// what Offered holds, and a plan is never accepted into auto or bypass.
	Set(ctx context.Context, mode policy.Mode, via string) error
}

// cliModes is the session's ModeController. It does not record mode.changed
// yet; the governance track adds that, and the dialog /mode auto needs.
type cliModes struct {
	st  *cliState
	pol *policy.Engine
}

var _ ModeController = (*cliModes)(nil)

func (m *cliModes) Offered() []policy.Mode {
	var out []policy.Mode
	for _, name := range m.st.appCfg.ModeCycle() {
		if _, err := m.st.appCfg.Apply(config.Overrides{Mode: name}); err == nil {
			out = append(out, policy.Mode(name))
		}
	}
	return out
}

func (m *cliModes) Cycle() (policy.Mode, error) {
	offered := m.Offered()
	if len(offered) == 0 {
		return m.pol.Mode, errors.New("the managed configuration leaves no mode to cycle to")
	}
	next := offered[0]
	if i := slices.Index(offered, m.pol.Mode); i >= 0 {
		next = offered[(i+1)%len(offered)]
	}
	if next == m.pol.Mode {
		return next, nil
	}
	return next, m.Set(context.Background(), next, agent.ViaShiftTab)
}

func (m *cliModes) Set(_ context.Context, mode policy.Mode, via string) error {
	switch via {
	case agent.ViaFlag:
		// The flag alone may choose bypass; the managed configuration still refuses it.
		if !validMode(string(mode)) {
			return fmt.Errorf("unknown mode %q", mode)
		}
		if _, err := m.st.appCfg.Apply(config.Overrides{Mode: string(mode)}); err != nil {
			return err
		}
		m.pol.Mode = mode
		return nil
	case agent.ViaShiftTab:
		if !slices.Contains(m.Offered(), mode) {
			return fmt.Errorf("%s is not in the Shift-Tab cycle", mode)
		}
	case agent.ViaPlanExit:
		if mode == policy.ModeAuto || mode == policy.ModeBypass {
			return fmt.Errorf("a plan is never accepted into %s", mode)
		}
	case agent.ViaSlash:
	default:
		return fmt.Errorf("unknown way to change mode %q", via)
	}
	if mode == policy.ModeBypass {
		return errors.New("bypass is chosen only at startup, with -mode bypass")
	}
	return switchMode(m.st.appCfg, m.pol, string(mode))
}
