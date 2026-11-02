//go:build !windows

package app

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// A repository can ship .abhed/config.json.tmp as a link to a file outside
// it. Raising the workspace's tier must not write through that link.
func TestRaiseWorkspaceTierDoesNotWriteThroughAPlantedLink(t *testing.T) {
	ws, outside := t.TempDir(), filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, ".abhed", "config.json.tmp")); err != nil {
		t.Fatal(err)
	}
	path, raised, err := raiseWorkspaceTier(ws, sandbox.TierContainer)
	if err != nil || !raised {
		t.Fatalf("raise: %v %v", raised, err)
	}
	if got, _ := os.ReadFile(outside); string(got) != "keep me\n" {
		t.Fatalf("the file the link names was overwritten: %q", got)
	}
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("config.json: %v %v", fi, err)
	}
	ents, _ := os.ReadDir(filepath.Join(ws, ".abhed"))
	for _, e := range ents {
		if e.Name() != "config.json" && e.Name() != "config.json.tmp" {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

// A config.json that is a pipe or a link is refused at once: never waited
// on, followed, or written through.
func TestRaiseWorkspaceTierRefusesAConfigThatIsNotAPlainFile(t *testing.T) {
	for _, kind := range []string{"fifo", "link to a file", "link to /dev/zero"} {
		t.Run(kind, func(t *testing.T) {
			ws, outside := t.TempDir(), filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(outside, []byte("keep me\n"), 0o600); err != nil {
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
			case "link to a file":
				err = os.Symlink(outside, cfg)
			default:
				err = os.Symlink("/dev/zero", cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, _, err := raiseWorkspaceTier(ws, sandbox.TierContainer); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("a config that is not a plain file was taken")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("raiseWorkspaceTier waited on the config")
			}
			if got, _ := os.ReadFile(outside); string(got) != "keep me\n" {
				t.Fatalf("the link's target was changed: %q", got)
			}
		})
	}
}
