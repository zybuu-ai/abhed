//go:build unix

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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

// A process is stopped for the tree only once its parent is checked to be in
// it; one whose parent is not is left running, never signalled for good.
func TestTreeStopsOnlyProcessesWhoseParentIsInIt(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the process table is read on Linux and macOS only")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	state := func() string {
		out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		return strings.TrimSpace(string(out))
	}
	if _, ok := stopMember(pid, map[int]bool{os.Getpid() + 1: true}); ok {
		t.Fatal("a process whose parent is not in the tree was taken into it")
	}
	if s := state(); strings.HasPrefix(s, "T") || s == "" {
		t.Fatalf("the process outside the tree was left stopped or ended: %q", s)
	}
	m, ok := stopMember(pid, map[int]bool{os.Getpid(): true})
	if !ok || !strings.HasPrefix(state(), "T") {
		t.Fatalf("a child of the tree was not stopped: %v %q", ok, state())
	}
	m.kill()
	if err := cmd.Wait(); err == nil {
		t.Fatal("the stopped member was not killed")
	}
}
