//go:build unix

package sandbox

import (
	"context"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A cancel kills a whole group only when the command leads it; a command in
// another group is killed alone, and that group is never signalled.
func TestCancelKillsOnlyAGroupTheCommandLeads(t *testing.T) {
	var mu sync.Mutex
	var signalled []int
	real := killGroup
	killGroup = func(pgid int) error {
		mu.Lock()
		signalled = append(signalled, pgid)
		mu.Unlock()
		return real(pgid)
	}
	t.Cleanup(func() { killGroup = real })

	for _, leads := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		cmd := EndWithCommand(exec.CommandContext(ctx, "sleep", "30"))
		// Not leading, it stays in this test's own group, which must never be signalled.
		cmd.SysProcAttr.Setsid = leads
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		if pgid, _ := syscall.Getpgid(pid); (pgid == pid) != leads {
			_ = cmd.Process.Kill()
			t.Fatalf("leads=%v but the command's group is %d (pid %d)", leads, pgid, pid)
		}
		mu.Lock()
		signalled = nil
		mu.Unlock()
		start := time.Now()
		cancel()
		_ = cmd.Wait()
		if took := time.Since(start); took > time.Second {
			t.Errorf("leads=%v: the cancel took %v", leads, took)
		}
		mu.Lock()
		got := append([]int(nil), signalled...)
		mu.Unlock()
		want := 0
		if leads {
			want = 1
		}
		if len(got) != want || (leads && got[0] != pid) {
			t.Errorf("leads=%v: groups signalled %v, want only the command's own (%d) when it leads one", leads, got, pid)
		}
	}
}
