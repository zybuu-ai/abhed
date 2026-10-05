//go:build unix

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
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
// With no terminal, a /dev/tty read fails at once instead of stopping unseen.
func EndWithCommand(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	// Every process the command starts inherits this, a daemon that left its
	// session and its parent included, so a cancel can still find it.
	marker := commandMarker()
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, marker)
	backend := cmd.Cancel
	cmd.Cancel = func() error {
		defer endMarked(marker)
		// A reaped leader's group id could be reused, so none is signalled once
		// Wait has reaped it; a cancel racing that reap is the window left.
		if errors.Is(cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone) {
			return os.ErrProcessDone
		}
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

// endTree stops root and its descendants pass after pass, a setsid child included
// while its parent lives, then kills them; an orphan reparented elsewhere is not
// reached. Each is stopped only once its parent is checked to be in the tree.
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

// markerName is the variable that marks every process of one command.
const markerName = "ABHED_COMMAND_ID"

// commandMarker is a fresh markerName=value, unguessable, for one command.
func commandMarker() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return markerName + "=" + hex.EncodeToString(b[:])
}

// endMarked stops, pass after pass, every process of this user that carries
// marker, then kills them: what a command left running after its parent and
// its session were gone. One that cleared its environment is not found.
func endMarked(marker string) {
	stopped := map[int]bool{}
	for range maxSweepPasses {
		fresh := 0
		for _, pid := range marked(marker) {
			if !stopped[pid] && syscall.Kill(pid, stopSignal) == nil {
				stopped[pid] = true
				fresh++
			}
		}
		if fresh == 0 {
			break
		}
	}
	// Read again once stopped: a pid freed and taken by another process in
	// between no longer carries the marker, and is continued, not killed.
	still := map[int]bool{}
	for _, pid := range marked(marker) {
		still[pid] = true
	}
	for pid := range stopped {
		if still[pid] {
			_ = syscall.Kill(pid, killSignal)
		} else {
			_ = syscall.Kill(pid, syscall.SIGCONT)
		}
	}
}
