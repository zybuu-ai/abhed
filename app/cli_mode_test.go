package app

import (
	"context"
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

func modesFor(cfg config.Config, start policy.Mode) (*cliModes, *policy.Engine) {
	pol := policy.New(start)
	return &cliModes{st: &cliState{appCfg: cfg}, pol: pol}, pol
}

// Shift-Tab never reaches auto or bypass, however often it is pressed and
// wherever it starts, and the managed configuration can only take modes out.
func TestModeCycleNeverReachesAutoOrBypass(t *testing.T) {
	pinned := config.Default()
	pinned.Managed, pinned.ManagedKeys, pinned.Permissions.Mode = true, []string{"permissions.mode"}, "default"
	narrowed := config.Default()
	narrowed.CLI.ModeCycle = []string{"plan", "default"}
	planOnly := config.Default()
	planOnly.CLI.ModeCycle = []string{"plan"}

	for _, c := range []struct {
		name    string
		cfg     config.Config
		start   policy.Mode
		offered []policy.Mode
		first   policy.Mode
	}{
		{"default cycle", config.Default(), policy.ModeDefault,
			[]policy.Mode{policy.ModeDefault, policy.ModeAcceptEdits, policy.ModePlan}, policy.ModeAcceptEdits},
		{"from auto", config.Default(), policy.ModeAuto,
			[]policy.Mode{policy.ModeDefault, policy.ModeAcceptEdits, policy.ModePlan}, policy.ModeDefault},
		{"from bypass", config.Default(), policy.ModeBypass,
			[]policy.Mode{policy.ModeDefault, policy.ModeAcceptEdits, policy.ModePlan}, policy.ModeDefault},
		{"managed cycle leaves accept-edits out", narrowed, policy.ModeDefault,
			[]policy.Mode{policy.ModeDefault, policy.ModePlan}, policy.ModePlan},
		{"managed mode refuses accept-edits", pinned, policy.ModeDefault,
			[]policy.Mode{policy.ModeDefault, policy.ModePlan}, policy.ModePlan},
		{"one mode stays put", planOnly, policy.ModePlan, []policy.Mode{policy.ModePlan}, policy.ModePlan},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, pol := modesFor(c.cfg, c.start)
			if got := m.Offered(); !slices.Equal(got, c.offered) {
				t.Fatalf("offered %v, want %v", got, c.offered)
			}
			for i := range 20 {
				got, err := m.Cycle()
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 && got != c.first {
					t.Fatalf("first press went to %s, want %s", got, c.first)
				}
				if got != pol.Mode || !slices.Contains(c.offered, got) || got == policy.ModeAuto || got == policy.ModeBypass {
					t.Fatalf("press %d reached %s (engine %s)", i+1, got, pol.Mode)
				}
			}
		})
	}
}

// Each way in keeps its own limits: bypass only from the flag and never under
// a managed file, Shift-Tab only within the cycle, a plan never into auto.
func TestModeSetRules(t *testing.T) {
	managed := config.Default()
	managed.Managed = true
	for _, c := range []struct {
		name string
		cfg  config.Config
		mode policy.Mode
		via  string
		ok   bool
	}{
		{"slash auto", config.Default(), policy.ModeAuto, agent.ViaSlash, true},
		{"slash bypass", config.Default(), policy.ModeBypass, agent.ViaSlash, false},
		{"slash unknown", config.Default(), "yolo", agent.ViaSlash, false},
		{"shift-tab auto", config.Default(), policy.ModeAuto, agent.ViaShiftTab, false},
		{"shift-tab plan", config.Default(), policy.ModePlan, agent.ViaShiftTab, true},
		{"plan-exit auto", config.Default(), policy.ModeAuto, agent.ViaPlanExit, false},
		{"plan-exit bypass", config.Default(), policy.ModeBypass, agent.ViaPlanExit, false},
		{"plan-exit accept-edits", config.Default(), policy.ModeAcceptEdits, agent.ViaPlanExit, true},
		{"flag bypass", config.Default(), policy.ModeBypass, agent.ViaFlag, true},
		{"flag bypass under a managed file", managed, policy.ModeBypass, agent.ViaFlag, false},
		{"unknown way", config.Default(), policy.ModePlan, "hook", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, pol := modesFor(c.cfg, policy.ModeDefault)
			err := m.Set(context.Background(), c.mode, c.via)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok %v", err, c.ok)
			}
			want := policy.ModeDefault
			if c.ok {
				want = c.mode
			}
			if pol.Mode != want {
				t.Fatalf("mode is %s, want %s", pol.Mode, want)
			}
		})
	}
}
