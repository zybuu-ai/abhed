package sandbox

import "testing"

// Only a tier holding git's configuration is brokered for, and only macOS's
// profile holds a repository made during the session against commands
// already running; bound tiers hold it from each command's start.
func TestHoldsGitAndHoldsNewGit(t *testing.T) {
	for _, c := range []struct {
		sb            Sandbox
		holds, newGit bool
	}{
		{&Process{policy: Policy{ProtectGit: true}, backend: "sandbox-exec"}, true, true},
		{&Process{policy: Policy{ProtectGit: true}, backend: "bwrap"}, true, false},
		{&Process{policy: Policy{}, backend: "sandbox-exec"}, false, false},
		{&Fence{policy: Policy{ProtectGit: true}, mounts: true}, true, false},
		{&Fence{policy: Policy{ProtectGit: true}}, false, false},
		{&Container{policy: Policy{ProtectGit: true}}, false, false},
	} {
		if HoldsGit(c.sb) != c.holds || HoldsNewGit(c.sb) != c.newGit {
			t.Errorf("%T %+v: holds %v new %v", c.sb, c.sb, HoldsGit(c.sb), HoldsNewGit(c.sb))
		}
	}
}
