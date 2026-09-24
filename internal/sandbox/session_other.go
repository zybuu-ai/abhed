//go:build !linux && !darwin

package sandbox

import (
	"errors"
	"syscall"
)

// On other platforms a session cannot be read, so nothing is swept.
const (
	stopSignal = syscall.Signal(0)
	killSignal = syscall.Signal(0)
)

func startTime(int) (uint64, bool) { return 0, false }

func waitExited(int) error { return errors.New("not supported") }

func sessionMembers(int) []int { return nil }

func signalMember(int, int, syscall.Signal) bool { return false }
