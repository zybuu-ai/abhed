package config

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/extension"
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
// never a managed-only one, never turns auto memory on, and never imports deeper.
func TestTrustedWorkspaceCannotMakeManagedOnlySettings(t *testing.T) {
	_, ws := trustHome(t, "", `{
	  "commands":{"dirs":["./cmds"]}, "statusline":{"command":"./s.sh"},
	  "memory":{"auto":true,"import_depth":9},
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
	if cfg.Memory.Auto || cfg.MemoryImportDepth() != 5 || len(cfg.CLI.ModeCycle) > 0 || cfg.Record.Dir != "" || cfg.Record.RetentionDays != 0 || cfg.Hooks.Disabled {
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
	want := []string{"cli.mode_cycle", "hooks.disabled", "memory.auto", "memory.import_depth", "record.dir", "record.retention_days"}
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

	for _, bad := range []string{`{"cli":{"mode_cycle":["auto"]}}`, `{"cli":{"mode_cycle":["default","bypass"]}}`, `{"record":{"retention_days":-1}}`,
		`{"memory":{"import_depth":11}}`, `{"memory":{"import_depth":1000000000}}`, `{"memory":{"import_depth":-1}}`} {
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
	for _, k := range []string{"cli.mode_cycle", "record.dir", "record.retention_days", "hooks.disabled", "hooks.managed_only"} {
		if !ManagedOnly(k) {
			t.Errorf("%s is not managed only", k)
		}
	}
	if ManagedOnly("memory.auto") || ManagedOnly("commands.dirs") {
		t.Error("a user setting is marked managed only")
	}
}

// The deepest allowed import chain loads.
func TestImportDepthCap(t *testing.T) {
	withManaged(t, `{"memory":{"import_depth":10}}`)
	cfg, err := Load(t.TempDir())
	if err != nil || cfg.MemoryImportDepth() != 10 {
		t.Fatalf("depth %d, %v", cfg.MemoryImportDepth(), err)
	}
}

// hooks.disabled leaves an extension only the tools it provides: no hook
// event reaches it, and one with nothing else to do is not started.
func TestHooksDisabledKeepsOnlyTools(t *testing.T) {
	cfg := Default()
	cfg.Extensions = []ExtensionConfig{
		{Name: "all", Command: "x"},
		{Name: "guard", Command: "x", Events: []string{"tool_call", "user_prompt_submit"}},
		{Name: "both", Command: "x", Events: []string{"tool_call", "list_tools", "invoke_tool"}, Match: []string{"bash(git *)"}, Async: true},
	}
	on := cfg.ExtensionSpecs()
	if len(on) != 3 || on[2].Match[0] != "bash(git *)" || !on[2].Async {
		t.Fatalf("with hooks on: %+v", on)
	}
	cfg.Hooks.Disabled = true
	off := cfg.ExtensionSpecs()
	var names []string
	for _, s := range off {
		names = append(names, s.Name)
		for _, ev := range s.Events {
			if ev != "list_tools" && ev != "invoke_tool" {
				t.Fatalf("%s still takes %s", s.Name, ev)
			}
		}
		if len(s.Events) == 0 {
			t.Fatalf("%s takes every event", s.Name)
		}
	}
	if !slices.Equal(names, []string{"all", "both"}) {
		t.Fatalf("started %v", names)
	}
}

// hooks.managed_only sends hook events only to the managed file's extensions:
// the user's keep only their tools, or do not start, and the user's file
// cannot set it.
func TestHooksManagedOnly(t *testing.T) {
	user := `{"extensions":[{"name":"mine","command":"/bin/mine","events":["tool_call","list_tools","invoke_tool"]},
	  {"name":"veto","command":"/bin/veto","events":["tool_call"]}],"hooks":{"managed_only":true}}`
	_, ws := trustHome(t, user, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hooks.ManagedOnly || len(cfg.ExtensionSpecs()) != 2 {
		t.Fatalf("the user's file set hooks.managed_only: %+v", cfg.Hooks)
	}

	withManaged(t, `{"hooks":{"managed_only":true}}`)
	writeConfig(t, os.Getenv("HOME"), user)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	specs := cfg.ExtensionSpecs()
	if len(specs) != 1 || specs[0].Name != "mine" || len(specs[0].Events) != 2 {
		t.Fatalf("a user extension kept its hooks: %+v", specs)
	}
	// A caller's own extensions, as the SDK passes them, are narrowed the same way.
	sdk := []extension.Config{{Name: "sdk", Events: []extension.Event{extension.EvToolCall}}}
	if got := cfg.NarrowHooks(sdk); len(got) != 0 {
		t.Fatalf("a caller's extension kept its hooks: %+v", got)
	}

	withManaged(t, `{"hooks":{"managed_only":true},"extensions":[{"name":"org","command":"/bin/org","events":["tool_call"]}]}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if specs := cfg.ExtensionSpecs(); len(specs) != 1 || specs[0].Name != "org" || len(specs[0].Events) != 1 {
		t.Fatalf("the managed extension lost its hooks: %+v", specs)
	}
}

// A -settings file merges over the user's own as one of theirs: its
// settings apply and are hashed, a managed-only key is set aside, the
// managed file still wins, and under the allow lock its allow rules go.
func TestSettingsFile(t *testing.T) {
	_, ws := trustHome(t, `{"memory":{"import_depth":3}}`, "")
	body := []byte(`{"memory":{"import_depth":2},"hooks":{"managed_only":true},"permissions":{"allow":["bash(make *)"],"deny":["bash(curl *)"]}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true, Settings: body, SettingsName: "ci.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryImportDepth() != 2 || cfg.Hooks.ManagedOnly || cfg.Settings.Name != "ci.json" || len(cfg.Settings.SHA256) != 64 {
		t.Fatalf("settings: %+v %+v %+v", cfg.Memory, cfg.Hooks, cfg.Settings)
	}
	if cfg.RuleLayer("allow", "bash(make *)") != LayerSettings {
		t.Fatalf("layer %q", cfg.RuleLayer("allow", "bash(make *)"))
	}

	withManaged(t, `{"permissions":{"deny":["bash(rm *)"]},"memory":{"import_depth":1}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: body, SettingsName: "ci.json"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cfg.Permissions.Allow, "bash(make *)") || cfg.MemoryImportDepth() != 1 {
		t.Fatalf("the settings file beat the managed file: %v %d", cfg.Permissions.Allow, cfg.MemoryImportDepth())
	}
	found := false
	for _, k := range cfg.SetAside {
		found = found || k.File == "ci.json" && k.Value == "bash(make *)"
	}
	if !found {
		t.Fatalf("the dropped allow rule was not named: %+v", cfg.SetAside)
	}

	if _, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(`{"mode":`), SettingsName: "bad.json"}); err == nil {
		t.Fatal("a malformed settings file loaded")
	}
}

// Git extensions opted in load from the person's files, are checked by
// name, and are set aside when the managed file sets the permissions
// without naming its own.
func TestGitExtensionsOptIn(t *testing.T) {
	_, ws := trustHome(t, `{}`, "")
	body := []byte(`{"permissions":{"git_extensions":["lfs","flow"]}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true, Settings: body, SettingsName: "ci.json"})
	if err != nil || !slices.Equal(cfg.Permissions.GitExtensions, []string{"lfs", "flow"}) {
		t.Fatalf("opt-in: %v %v", cfg.Permissions.GitExtensions, err)
	}
	for _, bad := range []string{`["reset"]`, `["-c"]`, `["l fs"]`, `[""]`} {
		_, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(`{"permissions":{"git_extensions":` + bad + `}}`), SettingsName: "bad.json"})
		if err == nil || !strings.Contains(err.Error(), "git_extensions") {
			t.Errorf("git_extensions %s loaded: %v", bad, err)
		}
	}

	// An untrusted workspace cannot opt one in: the agent can write it.
	_, ws = trustHome(t, `{}`, `{"permissions":{"git_extensions":["wipe"]}}`)
	if cfg, err = LoadWith(ws, LoadOptions{Quiet: true}); err != nil || len(cfg.Permissions.GitExtensions) != 0 {
		t.Fatalf("an untrusted workspace opted in: %v %v", cfg.Permissions.GitExtensions, err)
	}

	withManaged(t, `{"permissions":{"deny":["bash(rm *)"]}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: body, SettingsName: "ci.json"})
	if err != nil || len(cfg.Permissions.GitExtensions) != 0 {
		t.Fatalf("under managed permissions: %v %v", cfg.Permissions.GitExtensions, err)
	}
	if !slices.ContainsFunc(cfg.SetAside, func(k SetAsideKey) bool {
		return k.Key == "permissions.git_extensions" && k.Value == "lfs" && k.File == "ci.json"
	}) {
		t.Fatalf("the dropped opt-in was not named: %+v", cfg.SetAside)
	}

	withManaged(t, `{"permissions":{"deny":["bash(rm *)"],"git_extensions":["lfs"]}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil || !slices.Equal(cfg.Permissions.GitExtensions, []string{"lfs"}) {
		t.Fatalf("the managed file's own opt-in: %v %v", cfg.Permissions.GitExtensions, err)
	}
}

// A trusted workspace may still make imports shallower.
func TestTrustedWorkspaceMayImportShallower(t *testing.T) {
	_, ws := trustHome(t, `{"memory":{"import_depth":8}}`, `{"memory":{"import_depth":2}}`)
	cfg, err := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if err != nil || cfg.MemoryImportDepth() != 2 {
		t.Fatalf("depth %d: %v", cfg.MemoryImportDepth(), err)
	}
	_, ws = trustHome(t, `{"memory":{"import_depth":8}}`, `{"memory":{"import_depth":9}}`)
	if cfg, _ = LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true}); cfg.MemoryImportDepth() != 8 {
		t.Fatalf("a trusted workspace raised the user's depth to %d", cfg.MemoryImportDepth())
	}
}
