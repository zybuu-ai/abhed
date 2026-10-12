//go:build !unix

package sandbox

import "os/exec"

// ownGroup leaves cmd as it is: a cancel kills the process alone.
func ownGroup(*exec.Cmd) {}

// EndLeft does nothing here: there is no marker to find processes by.
func EndLeft(*exec.Cmd) {}

func markerIn([]string) string { return "" }

func commandMarker() string { return "" }

func endMarked(string) {}

// EndWithCommand only bounds the wait here: without process groups, killing
// the command is all a cancel can do.
func EndWithCommand(cmd *exec.Cmd) *exec.Cmd {
	cmd.WaitDelay = commandWaitDelay
	return cmd
}
