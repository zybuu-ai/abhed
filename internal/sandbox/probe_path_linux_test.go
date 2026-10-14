package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The process tier's probes run the system's tools, so Abhed started with a
// PATH holding bwrap and nothing else keeps the tier rather than fall to none.
func TestProcessProbesNeedNoToolsOnPath(t *testing.T) {
	if ok, why := NewProcess(DefaultPolicy(t.TempDir())).Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(bwrap, filepath.Join(bin, "bwrap")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if ok, why := NewProcess(DefaultPolicy(t.TempDir())).Available(); !ok {
		t.Fatalf("with only bwrap on PATH the process tier is unavailable: %s", why)
	}
}

// Under bubblewrap a server's program in the temporary folder, which a
// confined server does not see, is refused with that reason rather than left
// to start and close its pipe.
func TestServerInTheTemporaryFolderIsRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := NewProcess(DefaultPolicy(workspace(t)))
	if ok, why := s.Available(); !ok || s.Backend() != "bwrap" {
		t.Skipf("bubblewrap unavailable: %s", why)
	}
	dir, err := os.MkdirTemp("/tmp", "abhed-srv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	prog := filepath.Join(dir, "srv")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = s.ServerCommand(t.Context(), "x", []string{prog}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "temporary folder") {
		t.Fatalf("a program in the hidden temp: %v", err)
	}
}
