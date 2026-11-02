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

// A repository chooses what is at .abhed/config.json. A pipe, a link to
// /dev/zero or any link, or a file too large, is refused at once rather than
// waited on, read without end or followed.
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
				err = os.WriteFile(cfg, []byte(`{"_pad":"`+strings.Repeat("x", maxWorkspaceConfig)+`"}`), 0o600)
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
