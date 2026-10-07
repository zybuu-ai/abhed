package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A command signals only its own processes: one outside its sandbox, as
// Abhed is to it (kill $PPID), or another command's, survives the signal.
func TestSeatbeltSignalsStayInside(t *testing.T) {
	ws := workspace(t)
	s := NewProcess(DefaultPolicy(ws))
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	outside := exec.Command("/bin/sleep", "30")
	if err := outside.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- outside.Wait() }()
	t.Cleanup(func() { _ = outside.Process.Kill() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	script := fmt.Sprintf("kill -TERM %d && echo OUTSIDE_SIGNALLED || echo OUTSIDE_REFUSED; "+
		"sleep 30 & kill -TERM $! && echo OWN_SIGNALLED", outside.Process.Pid)
	out, _ := s.Command(ctx, ws, script).CombinedOutput()
	if !strings.Contains(string(out), "OUTSIDE_REFUSED") || !strings.Contains(string(out), "OWN_SIGNALLED") {
		t.Fatalf("signals:\n%s", out)
	}
	select {
	case err := <-exited:
		t.Fatalf("ESCAPE: the process outside the sandbox was signalled: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := outside.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the process outside is gone: %v", err)
	}
}
