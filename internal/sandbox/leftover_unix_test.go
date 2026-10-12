//go:build unix

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// What a command left running after it exited, in a session of its own, is
// ended by EndLeft through the marker every process of the command carries.
func TestEndLeftEndsWhatOutlivedTheCommand(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := EndWithCommand(exec.CommandContext(t.Context(), "/bin/sh", "-c", "( /bin/sleep 60 </dev/null >/dev/null 2>&1 & echo $! > "+pidFile+" )"))
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	var pid int
	for range 50 {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, _ = strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid <= 0 {
		t.Skip("the child's pid was not written")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if syscall.Kill(pid, 0) != nil {
		t.Skip("the child is already gone")
	}
	EndLeft(cmd)
	for range 100 {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the process the command left running is still there")
}
