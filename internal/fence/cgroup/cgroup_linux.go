package cgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Discover finds the delegated cgroup v2 subtree this process may manage: its
// own cgroup, or the one above it when Abhed already moved itself into the
// host leaf. It returns an error wrapping ErrNotDelegated, naming the way to
// get one, when the cgroup is the hierarchy's root or not this user's to
// write; the caller must then refuse to run, not run unbounded.
func Discover() (*Base, error) {
	procCgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotDelegated, err)
	}
	own, err := ownPath(string(procCgroup))
	if err != nil {
		return nil, notDelegated("/proc/self/cgroup", "%v", err)
	}
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotDelegated, err)
	}
	point, root, err := mountOf(string(mountinfo))
	if err != nil {
		return nil, notDelegated(own, "%v", err)
	}
	dir, err := dirOf(point, root, own)
	if err != nil {
		return nil, notDelegated(own, "%v", err)
	}
	if filepath.Base(dir) == hostLeaf {
		dir = filepath.Dir(dir)
	}
	if err := delegated(dir); err != nil {
		return nil, err
	}
	return &Base{dir: dir}, nil
}

// delegated checks this user may manage dir: make cgroups in it, move
// processes through it and enable controllers below it.
func delegated(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "cgroup.type")); err != nil {
		// Only the hierarchy's root lacks cgroup.type; it is the whole host's.
		return notDelegated(dir, "it is the root of the cgroup hierarchy, which Abhed does not manage")
	}
	for _, p := range []string{dir, filepath.Join(dir, "cgroup.procs"), filepath.Join(dir, "cgroup.subtree_control")} {
		if err := unix.Access(p, unix.W_OK); err != nil {
			return notDelegated(dir, "%s is not writable by this user (%v)", p, err)
		}
	}
	return nil
}

// Place makes cmd start inside the leaf: the kernel creates its first process
// there (CLONE_INTO_CGROUP), so nothing it runs is ever outside. The rest of
// cmd.SysProcAttr is kept. Call it before cmd.Start and remove the leaf only
// after Start returns. A kernel without clone3 fails the Start.
func (l *Leaf) Place(cmd *exec.Cmd) error {
	if cmd.Process != nil {
		return errors.New("cgroup: place: the command has already started")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fd == nil {
		return fmt.Errorf("cgroup: place: leaf %s was removed", l.id)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(l.fd.Fd()) // #nosec G115 -- a file descriptor fits an int
	return nil
}

// killPid sends SIGKILL to pid.
func killPid(pid int) error { return unix.Kill(pid, unix.SIGKILL) }

// ownerOf is the user that owns process pid.
func ownerOf(pid int) (int, bool) {
	var st unix.Stat_t
	if unix.Stat(fmt.Sprintf("/proc/%d", pid), &st) != nil {
		return 0, false
	}
	return int(st.Uid), true
}
