package microvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A watchdog whose driver ends without saying so kills the VM's cgroup; one
// told the driver ended the VM leaves it, and odd names are refused.
func TestWatchdogKillsTheCgroupWhenTheDriverDies(t *testing.T) {
	old := cgroupRoot
	t.Cleanup(func() { cgroupRoot = old })
	cgroupRoot = t.TempDir()
	dir := filepath.Join(cgroupRoot, "abhed-microvm", "vm1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := runWatchdog("abhed-microvm", "vm1", strings.NewReader("q")); code != 0 {
		t.Fatalf("told: exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "cgroup.kill")); err == nil {
		t.Fatal("a VM the driver ended itself was killed again")
	}
	if code := runWatchdog("abhed-microvm", "vm1", strings.NewReader("")); code != 0 {
		t.Fatalf("driver gone: exit %d", code)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cgroup.kill")); err != nil || string(b) != "1" {
		t.Fatalf("the cgroup was not killed: %q %v", b, err)
	}
	if code := runWatchdog("../etc", "vm1", strings.NewReader("")); code != 2 {
		t.Fatalf("a parent with a path in it: exit %d", code)
	}
	if code := runWatchdog("abhed-microvm", "../../x", strings.NewReader("")); code != 2 {
		t.Fatalf("a vm id with a path in it: exit %d", code)
	}
}
