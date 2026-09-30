package config

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// What a workspace file may do with the CLI's settings, untrusted: the
// instruction and process sources wait for trust, the managed-only keys are
// never taken, and auto memory and import depth only tighten.
func TestUntrustedWorkspaceCLIKeys(t *testing.T) {
	for _, c := range []struct {
		name, user, file, key string
		applied               bool
		took                  func(Config) bool
	}{
		{"commands dirs", "", `{"commands":{"dirs":["./cmds"]}}`, "commands.dirs", false, func(c Config) bool { return len(c.Commands.Dirs) > 0 }},
		{"rules dirs", "", `{"rules":{"dirs":["./rules"]}}`, "rules.dirs", false, func(c Config) bool { return len(c.Rules.Dirs) > 0 }},
		{"statusline", "", `{"statusline":{"command":"./status.sh"}}`, "statusline.command", false, func(c Config) bool { return c.Statusline.Command != "" }},
		{"auto memory on", "", `{"memory":{"auto":true}}`, "memory.auto", false, func(c Config) bool { return c.Memory.Auto }},
		{"deeper imports", "", `{"memory":{"import_depth":9}}`, "memory.import_depth", false, func(c Config) bool { return c.Memory.ImportDepth == 9 }},
		{"mode cycle", "", `{"cli":{"mode_cycle":["plan"]}}`, "cli.mode_cycle", false, func(c Config) bool { return len(c.CLI.ModeCycle) > 0 }},
		{"record dir", "", `{"record":{"dir":"/tmp/elsewhere"}}`, "record.dir", false, func(c Config) bool { return c.Record.Dir != "" }},
		{"retention", "", `{"record":{"retention_days":1}}`, "record.retention_days", false, func(c Config) bool { return c.Record.RetentionDays != 0 }},
		{"hooks off", "", `{"hooks":{"disabled":true}}`, "hooks.disabled", false, func(c Config) bool { return c.Hooks.Disabled }},

		{"auto memory off", `{"memory":{"auto":true}}`, `{"memory":{"auto":false}}`, "memory.auto", true, func(c Config) bool { return !c.Memory.Auto }},
		{"shallower imports", "", `{"memory":{"import_depth":2}}`, "memory.import_depth", true, func(c Config) bool { return c.Memory.ImportDepth == 2 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, ws := trustHome(t, c.user, c.file)
			cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
			if err != nil {
				t.Fatal(err)
			}
			if c.applied {
				if !c.took(cfg) || !slices.Contains(cfg.Workspace.Applied, c.key) || ignored(cfg.Workspace, c.key) {
					t.Fatalf("the tightening %s did not apply: applied %v, ignored %v", c.key, cfg.Workspace.Applied, cfg.Workspace.Ignored)
				}
				return
			}
			if c.took(cfg) || !ignored(cfg.Workspace, c.key) {
				t.Fatalf("an untrusted file applied %s, or did not name it: %+v", c.key, cfg.Workspace.Ignored)
			}
		})
	}
}

// Trust takes the workspace's command, rule and statusline settings, but
// never a managed-only one, and never turns auto memory on.
func TestTrustedWorkspaceCannotMakeManagedOnlySettings(t *testing.T) {
	_, ws := trustHome(t, "", `{
	  "commands":{"dirs":["./cmds"]}, "statusline":{"command":"./s.sh"},
	  "memory":{"auto":true},
	  "cli":{"mode_cycle":["plan"]}, "record":{"dir":"/tmp/x","retention_days":3}, "hooks":{"disabled":true}}`)
	var warned bytes.Buffer
	warnOut = &warned
	t.Cleanup(func() { warnOut = os.Stderr })
	cfg, err := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Workspace.Trusted || len(cfg.Commands.Dirs) != 1 || cfg.Statusline.Command != "./s.sh" {
		t.Fatalf("trust did not apply the ordinary settings: %+v %+v", cfg.Commands, cfg.Statusline)
	}
	if cfg.Memory.Auto || len(cfg.CLI.ModeCycle) > 0 || cfg.Record.Dir != "" || cfg.Record.RetentionDays != 0 || cfg.Hooks.Disabled {
		t.Fatalf("a trusted workspace made a setting it may not: %+v %+v %+v %+v", cfg.Memory, cfg.CLI, cfg.Record, cfg.Hooks)
	}
	var keys []string
	for _, k := range cfg.SetAside {
		keys = append(keys, k.Key)
		if cfg.Sets(k.Key) {
			t.Errorf("%s is still reported as set", k.Key)
		}
	}
	slices.Sort(keys)
	want := []string{"cli.mode_cycle", "hooks.disabled", "memory.auto", "record.dir", "record.retention_days"}
	if !slices.Equal(keys, want) {
		t.Fatalf("set aside %v, want %v", keys, want)
	}
	for _, k := range want {
		if !strings.Contains(warned.String(), k) {
			t.Errorf("no warning names %s:\n%s", k, warned.String())
		}
	}
}

// The user's own file may turn auto memory on, but a managed-only setting
// there is set aside and named.
func TestUserFileManagedOnlySettings(t *testing.T) {
	_, ws := trustHome(t, `{"memory":{"auto":true,"import_depth":3},"record":{"retention_days":30},"hooks":{"disabled":true}}`, "")
	var warned bytes.Buffer
	warnOut = &warned
	t.Cleanup(func() { warnOut = os.Stderr })
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Memory.Auto || cfg.MemoryImportDepth() != 3 {
		t.Fatalf("the user's memory settings were not applied: %+v", cfg.Memory)
	}
	if cfg.Record.RetentionDays != 0 || cfg.Hooks.Disabled || cfg.Sets("record.retention_days") {
		t.Fatalf("the user's file made a managed-only setting: %+v %+v", cfg.Record, cfg.Hooks)
	}
	if !strings.Contains(warned.String(), "record.retention_days") || !strings.Contains(warned.String(), "hooks.disabled") {
		t.Fatalf("not warned:\n%s", warned.String())
	}
}

// The managed file makes the managed-only settings; the mode cycle can only
// lose modes, and a managed auto memory binds.
func TestManagedCLISettings(t *testing.T) {
	withManaged(t, `{"cli":{"mode_cycle":["plan","default"]},"record":{"dir":"/srv/records","retention_days":90},
	  "hooks":{"disabled":true},"memory":{"auto":false}}`)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Record.Dir != "/srv/records" || cfg.Record.RetentionDays != 90 || !cfg.Hooks.Disabled || len(cfg.SetAside) != 0 {
		t.Fatalf("managed settings: %+v %+v aside %v", cfg.Record, cfg.Hooks, cfg.SetAside)
	}
	if got := cfg.ModeCycle(); !slices.Equal(got, []string{"default", "plan"}) {
		t.Fatalf("mode cycle %v, want the default order less accept-edits", got)
	}
	on := true
	var me *ManagedError
	if _, err := cfg.Apply(Overrides{MemoryAuto: &on}); !errors.As(err, &me) || me.Key != "memory.auto" {
		t.Fatalf("turning managed auto memory on: %v", err)
	}
	off := false
	if _, err := cfg.Apply(Overrides{MemoryAuto: &off}); err != nil {
		t.Fatalf("the managed value itself was refused: %v", err)
	}

	for _, bad := range []string{`{"cli":{"mode_cycle":["auto"]}}`, `{"cli":{"mode_cycle":["default","bypass"]}}`, `{"record":{"retention_days":-1}}`} {
		withManaged(t, bad)
		if _, err := Load(t.TempDir()); err == nil {
			t.Errorf("%s loaded", bad)
		}
	}
}

// Without a managed file the cycle is default, accept-edits, plan, and auto
// memory can be turned on and off.
func TestModeCycleAndMemoryDefaults(t *testing.T) {
	cfg := Default()
	if got := cfg.ModeCycle(); !slices.Equal(got, DefaultModeCycle) || slices.Contains(got, "auto") || slices.Contains(got, "bypass") {
		t.Fatalf("default cycle %v", got)
	}
	if cfg.Memory.Auto || cfg.MemoryImportDepth() != 5 {
		t.Fatalf("defaults: %+v", cfg.Memory)
	}
	on := true
	got, err := cfg.Apply(Overrides{MemoryAuto: &on})
	if err != nil || !got.Memory.Auto {
		t.Fatalf("turning auto memory on: %v", err)
	}
	for _, k := range []string{"cli.mode_cycle", "record.dir", "record.retention_days", "hooks.disabled"} {
		if !ManagedOnly(k) {
			t.Errorf("%s is not managed only", k)
		}
	}
	if ManagedOnly("memory.auto") || ManagedOnly("commands.dirs") {
		t.Error("a user setting is marked managed only")
	}
}
