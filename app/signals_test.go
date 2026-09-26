//go:build unix

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// TestHangupHelper, run on a terminal by the test below, runs one long command
// as the CLI does, under the CLI's stop signals.
func TestHangupHelper(t *testing.T) {
	pidFile := os.Getenv("ABHED_HANGUP_HELPER")
	if pidFile == "" {
		t.Skip("run by TestHangupEndsTheCommand")
	}
	stopper := cancelOnStop(stopReturns)
	defer stopper.stop()
	ctx := stopper.ctx
	s, err := tools.NewSession(filepath.Dir(pidFile))
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"command": `sh -c 'echo $$ > ` + pidFile + `; exec sleep 60'; true`, "description": "long"})
	res := tools.Bash{}.Run(ctx, s, args)
	fmt.Println("RESULT", res.Content)
}

// Closing the terminal ends the command the run was waiting on. It runs in a
// session of its own, so the hang-up reaches it only through the cancel.
func TestHangupEndsTheCommand(t *testing.T) {
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	pidFile := filepath.Join(dir, "child.pid")
	helper := exec.Command(os.Args[0], "-test.run=^TestHangupHelper$")
	helper.Env = append(os.Environ(), "ABHED_HANGUP_HELPER="+pidFile)
	tty, err := pty.Start(helper) // the helper leads a session with this terminal
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	// Nothing reads the terminal: a pending read would keep it from closing.
	t.Cleanup(func() {
		_ = helper.Process.Kill() // the helper this test started, by its pid
		_ = helper.Wait()
	})

	var pid int
	for deadline := time.Now().Add(20 * time.Second); pid == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	t.Cleanup(func() {
		// Only the sleep this test's helper started, and only while it still is one.
		if out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output(); err == nil &&
			strings.Contains(string(out), "sleep 60") {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	_ = tty.Close() // the terminal goes away, as when its window is closed
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the command %d outlived its terminal", pid)
		}
	}
}

// A draining command keeps catching stop signals, so a second SIGTERM cannot
// cut its drain short. The signals go to this test process, which is catching them.
func TestADrainIgnoresASecondSignal(t *testing.T) {
	stopper := cancelOnStop(stopDrains)
	defer stopper.stop()
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case <-stopper.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the first SIGTERM did not cancel")
	}
	if code, ok := stopCode(stopper.ctx); !ok || code != 128+int(syscall.SIGTERM) {
		t.Fatalf("stop code %d %v", code, ok)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond) // this process is still here to finish the test
}
