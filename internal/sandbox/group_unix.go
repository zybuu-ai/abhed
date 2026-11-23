//go:build unix

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// killGroup kills the process group pgid; replaced in tests.
var killGroup = killGroupNow

// sysKill is kill(2); replaced in tests of killGroupNow.
var sysKill = syscall.Kill

// killGroupNow signals only the target groupTarget allows.
func killGroupNow(pgid int) error {
	target, err := groupTarget(pgid)
	if err != nil {
		return err
	}
	return sysKill(target, syscall.SIGKILL)
}

// groupTarget is the kill(2) target for group pgid. It refuses 0 and 1 and a
// negative id, which would name the caller's own group, init's, or every process.
func groupTarget(pgid int) (int, error) {
	if pgid <= 1 || pgid == syscall.Getpgrp() {
		return 0, syscall.EINVAL
	}
	return -pgid, nil
}

// EndWithCommand runs cmd in a session of its own, which a cancel kills whole,
// together with every process descended from it.
func EndWithCommand(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	// Every process the command starts inherits this; a cancel ends one only
	// while it is also in the command's session (see endMarked).
	marker := commandMarker()
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, marker)
	backend := cmd.Cancel
	cmd.Cancel = func() error {
		// A reaped leader's group id could be reused, so none is signalled once
		// Wait has reaped it; a cancel racing that reap is the window left.
		if errors.Is(cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		// The marker only narrows the session this command created.
		since, _ := startTime(cmd.Process.Pid)
		defer endMarked(marker, since, inSession(cmd.Process.Pid))
		endTree(cmd.Process.Pid)
		// Only the group this command leads: never one it does not own.
		if pgid, gerr := syscall.Getpgid(cmd.Process.Pid); gerr != nil || pgid != cmd.Process.Pid {
			err := cmd.Process.Kill()
			if backend != nil {
				_ = backend()
			}
			return err
		}
		err := killGroup(cmd.Process.Pid)
		if backend != nil {
			_ = backend() // a container's removal, say; the kill is already done
		}
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = commandWaitDelay
	return cmd
}

// endTree stops then kills root and its descendants, each stopped only once its
// parent is checked to be in the tree.
func endTree(root int) {
	if syscall.Kill(root, stopSignal) != nil {
		return
	}
	tree := map[int]bool{root: true}
	var members []treeMember
	for range maxSweepPasses {
		fresh := 0
		for grew := true; grew; {
			grew = false
			for pid, ppid := range processParents() {
				if tree[pid] || !tree[ppid] {
					continue
				}
				if m, ok := stopMember(pid, tree); ok {
					tree[pid], grew = true, true
					members = append(members, m)
					fresh++
				}
			}
		}
		if fresh == 0 {
			break
		}
	}
	for _, m := range members {
		m.kill()
	}
}

// ownGroup starts cmd in a process group of its own, which a cancel kills
// whole: a helper the command started cannot keep its pipes open.
func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if err := killGroup(cmd.Process.Pid); err != nil && !errors.Is(err, syscall.ESRCH) {
			return cmd.Process.Kill()
		}
		return nil
	}
}

// markerIn is the last marker in env, or "".
func markerIn(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], markerName+"=") {
			return env[i]
		}
	}
	return ""
}

// commandMarker is a fresh markerName=value, unguessable, for one command.
func commandMarker() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return markerName + "=" + hex.EncodeToString(b[:])
}

// markedPids lists the processes started at or after since (a start time as
// startTime gives it; 0 for any) that carry a marker; replaced in tests.
var markedPids = marked

// endMarked stops then kills the processes carrying marker that signal agrees
// to signal. A marker is no proof: an app started from a command's terminal
// inherits it, and launchctl setenv can plant it in any app. So the marker only
// narrows: signal must itself prove each pid the command's own (a member of
// the session it created, or a process followed from its start) and refuse
// anything else. It returns how many marked processes were not proven and
// were left running, for the caller to report.
//
// Only processes started at or after since are read: one that was running
// before the command (an editor, say) cannot be its own.
func endMarked(marker string, since uint64, signal func(pid int, sig syscall.Signal) bool) (unproven int) {
	stopped, refused := map[int]bool{}, map[int]bool{}
	for range maxSweepPasses {
		fresh := 0
		for _, pid := range markedPids(marker, since) {
			if stopped[pid] {
				continue
			}
			if !signal(pid, stopSignal) {
				refused[pid] = true
				continue
			}
			stopped[pid], fresh = true, fresh+1
			delete(refused, pid)
		}
		if fresh == 0 {
			break
		}
	}
	// Read again once stopped: a pid no longer marked is continued, not killed.
	still := map[int]bool{}
	for _, pid := range markedPids(marker, since) {
		still[pid] = true
	}
	for pid := range stopped {
		if still[pid] {
			_ = signal(pid, killSignal)
		} else {
			_ = signal(pid, syscall.SIGCONT)
		}
	}
	for pid := range refused {
		if still[pid] {
			unproven++
		}
	}
	return unproven
}
