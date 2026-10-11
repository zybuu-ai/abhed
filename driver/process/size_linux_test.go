package process

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// On a host with a delegated cgroup, a box holds its command in a leaf of
// its own cgroup, with the memory bound written.
func TestHostSizerHoldsACommand(t *testing.T) {
	if err := hostSizer.Check(); err != nil {
		t.Skipf("no delegated cgroup here: %v", err)
	}
	b, err := hostSizer.NewBox("sizer-test", size{cpuPercent: 50, memoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Remove(context.Background()) })
	cmd := exec.Command("sh", "-c", "cat /proc/self/cgroup; cat /sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)/../memory.max")
	release, err := b.Place(cmd)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	release()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "abhed-sandbox-pd-sizer-test/c1") || !strings.Contains(string(out), "67108864") {
		t.Fatalf("the command ran outside its box: %s", out)
	}
}
