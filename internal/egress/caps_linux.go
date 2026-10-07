package egress

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// dropCaps clears every capability the sandbox granted the relay, on every
// thread, so nothing it runs or does afterwards holds one.
func dropCaps() error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	_, _, e := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0, 0)
	if e == unix.ENOTSUP {
		// A cgo build cannot reach every thread: this one, which is the
		// relay's main goroutine's, at least; releases are built without cgo.
		runtime.LockOSThread()
		if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
			return err
		}
		return unix.Capset(&hdr, &data[0])
	}
	if e != 0 && e != unix.EINVAL {
		return e
	}
	_, _, e = syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0) // #nosec G103 -- the capset ABI
	if e != 0 {
		return e
	}
	return nil
}

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
