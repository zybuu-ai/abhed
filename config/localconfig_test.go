package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeLocal(t *testing.T, ws, body string) string {
	t.Helper()
	p := filepath.Join(ws, ".abhed", LocalConfigName)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// An untrusted local layer only tightens, as the workspace file does.
func TestLocalConfigUntrustedOnlyTightens(t *testing.T) {
	_, ws := trustHome(t, "", "")
	local := writeLocal(t, ws, `{"permissions":{"mode":"bypass","deny":["bash(curl*)"]}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.Trusted || cfg.Workspace.File != local || cfg.Workspace.LocalFile != local {
		t.Fatalf("trust %+v", cfg.Workspace)
	}
	if cfg.Permissions.Mode == "bypass" || !slices.Contains(cfg.Permissions.Deny, "bash(curl*)") {
		t.Fatalf("mode %q deny %v", cfg.Permissions.Mode, cfg.Permissions.Deny)
	}
	if len(cfg.Workspace.Ignored) == 0 || cfg.Workspace.Ignored[0].Key != "permissions.mode" {
		t.Fatalf("ignored %+v", cfg.Workspace.Ignored)
	}
}

// Trusted, the local layer goes over the workspace file; it still never
// beats the managed file or makes a managed-only setting.
func TestLocalConfigTrustedOverridesButNotManaged(t *testing.T) {
	withManaged(t, `{"permissions":{"deny":["bash(rm *)"]},"limits":{"max_turns":40}}`)
	_, ws := trustHome(t, "", `{"limits":{"max_budget_tokens":5000},"permissions":{"mode":"plan"}}`)
	writeLocal(t, ws, `{"limits":{"max_budget_tokens":2000,"max_turns":400},"permissions":{"mode":"accept-edits","deny":[]},"control_plane":{"ha":{"enabled":true}}}`)
	cfg, err := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Workspace.Trusted {
		t.Fatalf("trust %+v", cfg.Workspace)
	}
	if cfg.Limits.MaxBudgetTokens != 2000 || cfg.Permissions.Mode != "accept-edits" {
		t.Fatalf("the local layer did not go over the workspace file: cost %v mode %q", cfg.Limits.MaxBudgetTokens, cfg.Permissions.Mode)
	}
	if cfg.Limits.MaxTurns != 40 || !slices.Contains(cfg.Permissions.Deny, "bash(rm *)") {
		t.Fatalf("the local layer loosened managed: turns %d deny %v", cfg.Limits.MaxTurns, cfg.Permissions.Deny)
	}
	if cfg.ControlPlane.HA.Enabled {
		t.Fatal("the local layer made an operator-only setting")
	}
}

// Trust covers both files: trust given for the workspace file alone does not
// reach a local layer added later, and a change to the local layer asks again.
func TestLocalConfigIsInTheTrustedContent(t *testing.T) {
	_, ws := trustHome(t, "", `{"permissions":{"mode":"plan"}}`)
	before, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := GrantTrust(before.Workspace.Workspace, before.Workspace.SHA256); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, ws, `{"permissions":{"mode":"bypass"}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.Trusted || cfg.Workspace.Reason != "changed" || cfg.Permissions.Mode == "bypass" {
		t.Fatalf("a new local layer rode on old trust: %+v mode %q", cfg.Workspace, cfg.Permissions.Mode)
	}
	paths, _, unchanged, err := cfg.Workspace.WorkspaceFiles()
	if err != nil || !unchanged || len(paths) != 2 {
		t.Fatalf("review files %v %v %v", paths, unchanged, err)
	}
}

// A local layer that leads out of the workspace is refused, as the
// workspace file is.
func TestLocalConfigLinkOutIsRefused(t *testing.T) {
	_, ws := trustHome(t, "", "")
	outside := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, ".abhed", LocalConfigName)); err != nil {
		t.Skip(err)
	}
	if _, err := LoadWith(ws, LoadOptions{Quiet: true}); err == nil {
		t.Fatal("a local layer linked out of the workspace was read")
	}
}

// init keeps the local layer out of the repository.
func TestInitIgnoresTheLocalLayer(t *testing.T) {
	_, ws := trustHome(t, "", "")
	if _, err := InitWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".abhed", ".gitignore"))
	if err != nil || string(data) != LocalConfigName+"\n" {
		t.Fatalf(".gitignore %q %v", data, err)
	}
}
