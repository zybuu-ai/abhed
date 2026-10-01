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

// A cancel signals only the group the command itself leads: its own pid as
// the group id, never 0, 1, -1 or the caller's group.
func TestCancelSignalsOnlyTheCommandsOwnGroup(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())
	cmd := EndWithCommand(exec.CommandContext(ctx, "sleep", "30"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err != nil || pgid != cmd.Process.Pid || pgid == syscall.Getpgrp() {
		t.Fatalf("the command is not in a group of its own: pgid %d, pid %d, ours %d", pgid, cmd.Process.Pid, syscall.Getpgrp())
	}
	cancel()
	_ = cmd.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(signalled) != 1 || signalled[0] != cmd.Process.Pid {
		t.Fatalf("signalled groups %v, want only %d", signalled, cmd.Process.Pid)
	}
}

// The kill target refuses every id that would reach beyond one child's group.
func TestGroupTargetRefusesWideTargets(t *testing.T) {
	for _, pgid := range []int{0, 1, -1, -42, syscall.Getpgrp()} {
		if target, err := groupTarget(pgid); err == nil {
			t.Fatalf("groupTarget(%d) = %d, want refused", pgid, target)
		}
	}
	if target, err := groupTarget(4242); err != nil || target != -4242 {
		t.Fatalf("groupTarget(4242) = %d, %v", target, err)
	}
}

// killGroup's own path goes through groupTarget: no wide id reaches kill(2),
// and a child's group is signalled as -pgid with SIGKILL.
func TestKillGroupSignalsOnlyTheTarget(t *testing.T) {
	type sent struct {
		pid int
		sig syscall.Signal
	}
	var calls []sent
	orig := sysKill
	sysKill = func(pid int, sig syscall.Signal) error { calls = append(calls, sent{pid, sig}); return nil }
	defer func() { sysKill = orig }()
	for _, pgid := range []int{0, 1, -1, -42, syscall.Getpgrp()} {
		if err := killGroupNow(pgid); err == nil {
			t.Fatalf("killGroupNow(%d) was not refused", pgid)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("refused ids reached kill(2): %v", calls)
	}
	if err := killGroupNow(4242); err != nil || len(calls) != 1 || calls[0] != (sent{-4242, syscall.SIGKILL}) {
		t.Fatalf("killGroupNow(4242): %v, calls %v", err, calls)
	}
}
