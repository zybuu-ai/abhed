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

// resolving stands in for a sandbox still being chosen (the command line's).
type resolving struct {
	Sandbox
	inner Sandbox
}

func (r resolving) Resolve() (Sandbox, error) { return r.inner, nil }

// A sandbox chosen in the background is judged as the one it stands for.
func TestHoldsGitSeesThroughALazySandbox(t *testing.T) {
	held := &Process{policy: Policy{ProtectGit: true}, backend: "sandbox-exec"}
	lazy := resolving{Sandbox: NewNone(Policy{}), inner: held}
	if !HoldsGit(lazy) || !HoldsNewGit(lazy) {
		t.Fatal("a lazily chosen sandbox that holds git is not seen to")
	}
	if HoldsGit(resolving{Sandbox: NewNone(Policy{}), inner: NewNone(Policy{})}) {
		t.Fatal("a lazily chosen none tier holds git")
	}
}
