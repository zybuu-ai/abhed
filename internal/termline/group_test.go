//go:build darwin || linux

package termline

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// A process is named as the kernel keeps it, with its parent and group.
func TestProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skip("cannot start sleep:", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	name, ppid, pgid, ok := Process(cmd.Process.Pid)
	if !ok || name != "sleep" || ppid != os.Getpid() || pgid != cmd.Process.Pid {
		t.Fatalf("got %q ppid %d pgid %d %v", name, ppid, pgid, ok)
	}
	if _, _, _, ok := Process(0); ok {
		t.Fatal("pid 0 named")
	}
}
