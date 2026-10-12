package process

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
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
	if !strings.Contains(string(out), "abhed-sandbox-pd-sizer-test/call-c1") || !strings.Contains(string(out), "67108864") {
		t.Fatalf("the command ran outside its box: %s", out)
	}
}

// A sized instance on the real process tier runs its commands, each in a
// cgroup of its own inside the instance's, before and after a restart.
func TestRealSizedInstanceRunsCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the host's sandbox")
	}
	if err := hostSizer.Check(); err != nil {
		t.Skipf("no delegated cgroup here: %v", err)
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("no bubblewrap here")
	}
	ctx := context.Background()
	d := New(config.Default())
	spec := driver.Spec{ID: "sized-real", Workspace: localDir(t), MinTier: "process", Config: map[string]string{"memory_mb": "256", "cpus": "1"}}
	if _, err := d.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Delete(context.Background(), spec.ID) })
	run := func(when string) {
		var out bytes.Buffer
		res, err := d.Exec(ctx, spec.ID, driver.ExecRequest{Command: "echo ran; cat /proc/self/cgroup", Stdout: &out})
		if err != nil || res.ExitCode != 0 || !strings.Contains(out.String(), "ran") {
			t.Fatalf("%s: a command in a sized instance: %v %+v %s", when, err, res, out.String())
		}
		// Seen from inside a cgroup namespace the path is "/"; otherwise it names the box.
		if cg := out.String(); strings.Contains(cg, "abhed-sandbox-") && !strings.Contains(cg, "abhed-sandbox-pd-sized-real/") {
			t.Fatalf("%s: the command ran outside the instance's cgroup: %s", when, cg)
		}
	}
	run("created")
	if _, err := d.Stop(ctx, spec.ID, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(ctx, spec.ID); err != nil {
		t.Fatal(err)
	}
	run("restarted")
}
