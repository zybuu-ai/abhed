//go:build unix && !linux && !darwin

package sandbox

// group_unix.go ends a tree through these; elsewhere no tree member is known.
type treeMember struct{}

func stopMember(int, map[int]bool) (treeMember, bool) { return treeMember{}, false }

func (treeMember) kill() {}

func marked(string) []int { return nil }
