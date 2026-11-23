package config

import "testing"

// A workspace file may turn git through the person's credentials off, never
// back on.
func TestHostGitOnlyTurnsOff(t *testing.T) {
	_, ws := trustHome(t, `{}`, `{"sandbox":{"host_git":false}}`)
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil || cfg.Sandbox.HostGit == nil || *cfg.Sandbox.HostGit {
		t.Fatalf("a workspace could not turn host git off: %v %v", cfg.Sandbox.HostGit, err)
	}
	_, ws = trustHome(t, `{"sandbox":{"host_git":false}}`, `{"sandbox":{"host_git":true}}`)
	cfg, err = LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil || cfg.Sandbox.HostGit == nil || *cfg.Sandbox.HostGit {
		t.Fatalf("a workspace turned host git back on: %v %v", cfg.Sandbox.HostGit, err)
	}
}
