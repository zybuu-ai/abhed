//go:build !unix

package sandbox

import "os/exec"

// EndWithCommand only bounds the wait here: without process groups, killing
// the command is all a cancel can do.
func EndWithCommand(cmd *exec.Cmd) *exec.Cmd {
	cmd.WaitDelay = commandWaitDelay
	return cmd
}
