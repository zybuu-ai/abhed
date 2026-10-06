//go:build linux

package landlock

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Detect asks the kernel which Landlock ABI it implements. It returns 0 and
// an error naming the cause when Landlock is missing or turned off.
func Detect() (ABI, error) {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		switch {
		case errors.Is(errno, unix.ENOSYS):
			return 0, fmt.Errorf("%w: this kernel has no Landlock (Linux 5.13 or later, built with it, is needed)", ErrUnsupported)
		case errors.Is(errno, unix.EOPNOTSUPP):
			return 0, fmt.Errorf("%w: this kernel has Landlock built in but turned off (add landlock to the lsm= boot parameter)", ErrUnsupported)
		}
		return 0, fmt.Errorf("%w: the kernel refused Landlock: %w", ErrUnsupported, errno)
	}
	return ABI(v), nil // #nosec G115 -- the kernel returns a small ABI number
}

// Restrict confines the calling thread to the ruleset's domain. It locks the
// calling goroutine to its OS thread for good, so the caller's exec runs from
// the confined thread and the command inherits the domain. Every step must
// succeed; on any error the caller must not run the command.
func (r *Ruleset) Restrict() error {
	// Landlock and no_new_privs bind the thread, not the process. The thread
	// is never unlocked: Go discards it when the goroutine ends.
	runtime.LockOSThread()
	if err := refuseRoot(unix.Getuid(), unix.Geteuid()); err != nil {
		return err
	}
	// The filesystem may have changed since New: a link could now hide an
	// overlap.
	if err := r.spec.Validate(); err != nil {
		return err
	}
	live, err := Detect()
	if err != nil {
		return err
	}
	if live < r.abi {
		return fmt.Errorf("%w: the ruleset needs Landlock ABI %d and this kernel has ABI %d", ErrUnsupported, int(r.abi), int(live))
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("setting no_new_privs: %w", err)
	}

	handled := r.abi.handledFS()
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	if r.spec.DenyTCP {
		attr.Access_net = netTCP
	}
	attr.Scoped = r.abi.scopes()
	// #nosec G103 -- the kernel ABI's struct, passed by address to the syscall that reads it
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("creating the Landlock ruleset: %w", errno)
	}
	ruleset := int(fd) // #nosec G115 -- a file descriptor fits in an int
	defer func() { _ = unix.Close(ruleset) }()

	for _, g := range []struct {
		paths  []string
		rights uint64
	}{
		{r.spec.Exec, execRights},
		{r.spec.Read, readRights},
		{r.spec.Devices, deviceRights},
		{r.spec.Write, handled},
	} {
		for _, p := range g.paths {
			if err := allowBeneath(ruleset, p, g.rights&handled); err != nil {
				return err
			}
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("entering the Landlock domain: %w", errno)
	}
	return nil
}

// allowBeneath grants rights on path and everything under it. A path that
// cannot be opened is left out: that only narrows the domain.
func allowBeneath(ruleset int, path string, rights uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil //nolint:nilerr // a missing or unreadable path is simply not granted
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("checking %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		rights &= fileRights
	}
	if rights == 0 {
		return nil
	}
	rule := unix.LandlockPathBeneathAttr{Allowed_access: rights, Parent_fd: int32(fd)} // #nosec G115 -- a file descriptor fits in an int32
	// #nosec G103 -- the kernel ABI's struct, passed by address to the syscall that reads it
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("granting %s: %w", path, errno)
	}
	return nil
}
