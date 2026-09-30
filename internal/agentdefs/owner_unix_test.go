//go:build unix

package agentdefs

import (
	"os"
	"syscall"
	"testing"
)

// Root's, and writable by nobody else: group- or other-writable is refused,
// another owner is refused, and a link's own mode is not held against it.
func TestRootOwnedNotSharedModes(t *testing.T) {
	root, user := &syscall.Stat_t{Uid: 0}, &syscall.Stat_t{Uid: 1000}
	for _, c := range []struct {
		mode os.FileMode
		sys  any
		want bool
	}{
		{0o644, root, true},
		{0o444, root, true},
		{0o664, root, false},
		{0o646, root, false},
		{os.ModeDir | 0o775, root, false},
		{os.ModeDir | 0o755, root, true},
		{os.ModeSymlink | 0o777, root, true},
		{0o644, user, false},
		{os.ModeSymlink | 0o777, user, false},
		{0o644, nil, false},
	} {
		if got := rootOwnedNotShared(fakeInfo{mode: c.mode, sys: c.sys}); got != c.want {
			t.Errorf("mode %v owner %v: %v, want %v", c.mode, c.sys, got, c.want)
		}
	}
}
