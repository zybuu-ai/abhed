package egress

import (
	"errors"
	"os"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// lastCap is past every capability a kernel defines; dropping one it lacks is EINVAL.
const lastCap = 64

// dropCaps clears the relay's capabilities on every thread, and with bounding its
// bounding set too, so a root command it then starts gains none at exec.
// A cgo build cannot reach every thread, so it fails rather than drop on one.
func dropCaps(bounding bool) error {
	all := func(trap, a1, a2 uintptr) syscall.Errno {
		_, _, e := syscall.AllThreadsSyscall6(trap, a1, a2, 0, 0, 0, 0)
		return e
	}
	if bounding {
		for c := uintptr(0); c < lastCap; c++ {
			if e := all(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, c); e != 0 && e != unix.EINVAL {
				return e
			}
		}
	}
	if e := all(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL); e != 0 && e != unix.EINVAL {
		return e
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	// Effective, permitted and inheritable, all empty.
	var data [2]unix.CapUserData
	if e := all(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0]))); e != 0 { // #nosec G103 -- the capset ABI
		return e
	}
	return nil
}

// undumpable keeps the command from attaching to or reading the relay while it holds capabilities.
func undumpable() error { return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) }

// nestedUser runs the command in a user namespace of its own as uid and gid, Abhed's
// user, where its capabilities reach nothing the sandbox owns; the relay's own ids need none.
func nestedUser(uid, gid string) (*syscall.SysProcAttr, error) {
	u, err1 := strconv.Atoi(uid)
	g, err2 := strconv.Atoi(gid)
	if err := errors.Join(err1, err2); err != nil || u < 0 || g < 0 {
		return nil, errors.New("the uid and gid are not numbers")
	}
	if u == os.Getuid() && g == os.Getgid() {
		return nil, nil
	}
	return &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: u, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: g, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}, nil
}
