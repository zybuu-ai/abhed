//go:build unix

package enginecli

import (
	"errors"
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in a process group of its own, which its cancel kills
// whole, so a helper the CLI started cannot outlive the query or hold its pipes.
func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		pgid := cmd.Process.Pid
		// Never the caller's own group, init's or every process.
		if pgid <= 1 || pgid == syscall.Getpgrp() {
			return cmd.Process.Kill()
		}
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return cmd.Process.Kill()
		}
		return nil
	}
}
