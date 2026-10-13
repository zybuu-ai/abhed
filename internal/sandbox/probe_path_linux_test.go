package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
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
