//go:build unix && !linux && !darwin

package sandbox

import "syscall"

// group_unix.go ends a tree through these; elsewhere no tree member is known.
type treeMember struct{}

func stopMember(int, map[int]bool) (treeMember, bool) { return treeMember{}, false }

func (treeMember) kill() {}

func marked(string, uint64) []int { return nil }

// processStarts and processOrigin are macOS's: here a process that lost its
// parent names no origin, and is followed only through its parent.
func processStarts() map[int]uint64 { return nil }

func processOrigin(int) (procOrigin, bool) { return procOrigin{}, false }

func pinProcess(int) (func(syscall.Signal) error, func(), error) { return nil, nil, syscall.ENOSYS }

func processName(int) string { return "" }
