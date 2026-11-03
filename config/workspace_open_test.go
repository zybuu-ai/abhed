//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A repository chooses what is at .abhed/config.json. A pipe, a link out of
// the workspace (to /dev/zero or to a file), or a file too large, is refused
// at once rather than waited on, read without end or followed.
func TestWorkspaceConfigMustBeAPlainFile(t *testing.T) {
	for _, kind := range []string{"fifo", "link to /dev/zero", "link to a file", "too large"} {
		t.Run(kind, func(t *testing.T) {
			withManaged(t, `{}`)
			ws, outside := t.TempDir(), filepath.Join(t.TempDir(), "other.json")
			if err := os.WriteFile(outside, []byte(`{"permissions":{"mode":"plan"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(ws, ".abhed", "config.json")
			if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "fifo":
				err = syscall.Mkfifo(cfg, 0o600)
			case "link to /dev/zero":
				err = os.Symlink("/dev/zero", cfg)
			case "link to a file":
				err = os.Symlink(outside, cfg)
			default:
				err = os.WriteFile(cfg, []byte(`{"_pad":"`+strings.Repeat("x", MaxWorkspaceConfig)+`"}`), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := LoadWith(ws, LoadOptions{Quiet: true}); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("a workspace config that is not a plain file was loaded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("loading waited on the workspace config")
			}
		})
	}
}

// In the home folder the file is the person's own, and may be a link to
// their dotfiles.
func TestHomeConfigMayBeALink(t *testing.T) {
	withManaged(t, `{}`)
	home := os.Getenv("HOME")
	real := filepath.Join(t.TempDir(), "dotfiles.json")
	if err := os.WriteFile(real, []byte(`{"limits":{"max_turns":7}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(home, ".abhed", "config.json")); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWith(home, LoadOptions{Quiet: true})
	if err != nil || cfg.Limits.MaxTurns != 7 {
		t.Fatalf("home config through a link: %v %d", err, cfg.Limits.MaxTurns)
	}
}

// abhed init replaces a config.json a repository made a link; it never
// writes through it.
func TestInitWorkspaceDoesNotWriteThroughALink(t *testing.T) {
	withManaged(t, `{}`)
	t.Setenv(TrustEnv, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ws, outside := t.TempDir(), filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, ".abhed", "config.json")); err != nil {
		t.Fatal(err)
	}
	path, err := InitWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(outside); string(got) != "keep me\n" {
		t.Fatalf("the link's target was overwritten: %q", got)
	}
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config.json: %v %v", fi, err)
	}
}

// A link to a plain file inside the workspace is followed (a config shared
// across a monorepo); one to a pipe inside it is still refused; and each
// refusal names the file once and says what to do.
func TestWorkspaceConfigLinkInsideTheWorkspace(t *testing.T) {
	withManaged(t, `{}`)
	ws := t.TempDir()
	shared := filepath.Join(ws, "shared", "abhed.json")
	if err := os.MkdirAll(filepath.Dir(shared), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte(`{"permissions":{"deny":["bash(curl*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(ws, ".abhed", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../shared/abhed.json", cfgPath); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatalf("a link inside the workspace: %v", err)
	}
	if cfg.Workspace.File == "" || !strings.Contains(strings.Join(cfg.Permissions.Deny, ","), "bash(curl*)") {
		t.Fatalf("the linked config was not read: %+v %v", cfg.Workspace, cfg.Permissions.Deny)
	}
	// A link inside the workspace to a pipe.
	pipe := filepath.Join(ws, "shared", "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(cfgPath)
	if err := os.Symlink("../shared/pipe", cfgPath); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadWith(ws, LoadOptions{Quiet: true}); done <- err }()
	select {
	case err := <-done:
		if err == nil || strings.Count(err.Error(), cfgPath) != 1 || !strings.Contains(err.Error(), "make it a plain JSON file") {
			t.Fatalf("a link to a pipe: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loading waited on a pipe behind a link")
	}
	// A link out of the workspace says so.
	_ = os.Remove(cfgPath)
	outside := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, cfgPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWith(ws, LoadOptions{Quiet: true}); err == nil || strings.Count(err.Error(), cfgPath) != 1 {
		t.Fatalf("a link out of the workspace: %v", err)
	}
}
