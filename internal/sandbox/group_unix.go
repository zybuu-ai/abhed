//go:build unix

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// killGroup kills the process group pgid; replaced in tests.
var killGroup = func(pgid int) error { return syscall.Kill(-pgid, syscall.SIGKILL) }

// EndWithCommand runs cmd in a session of its own, which a cancel kills whole,
// together with every process descended from it.
// With no terminal, a /dev/tty read fails at once instead of stopping unseen.
func EndWithCommand(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	backend := cmd.Cancel
	cmd.Cancel = func() error {
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
