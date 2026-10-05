//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// A runtime is Podman by what it says it is, whatever it is called: the
// podman-docker wrapper is named docker.
func TestSaysPodman(t *testing.T) {
	dir := t.TempDir()
	for name, says := range map[string]string{"docker": "podman version 5.2.0", "docker-real": "Docker version 27.1.1, build 6312585"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho '"+says+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got, want := saysPodman(p), name == "docker"; got != want {
			t.Errorf("%s saying %q: podman %v, want %v", name, says, got, want)
		}
	}
}
