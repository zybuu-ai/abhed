//go:build !linux && !darwin

package sandbox

// On other platforms the process table is not read: no tree is ended past
// the command's group, and no process bound is set.
func processParents() map[int]int { return nil }

func userProcesses() (int, bool) { return 0, false }

var procSoftLimit = func() (uint64, bool) { return 0, false }

type treeMember struct{}

func stopMember(int, map[int]bool) (treeMember, bool) { return treeMember{}, false }

func (treeMember) kill() {}
