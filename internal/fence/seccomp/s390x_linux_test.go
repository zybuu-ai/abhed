//go:build linux && s390x

package seccomp

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func addS390x(want map[string]uint32) {
	want["socketcall"] = unix.SYS_SOCKETCALL
	want["umount"] = unix.SYS_UMOUNT
}

// socketcallChecks make sockets through socketcall(2), whose arguments are
// an array in memory.
func socketcallChecks() []outcome {
	call := func(op uintptr, args ...uintptr) (uintptr, uintptr, unix.Errno) {
		var a [6]uintptr
		copy(a[:], args)
		return unix.RawSyscall(unix.SYS_SOCKETCALL, op, uintptr(unsafe.Pointer(&a[0])), 0)
	}
	var fds [2]int32
	pair := errnoErr(callErrno(call(8 /* SYS_SOCKETPAIR */, unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0, uintptr(unsafe.Pointer(&fds[0])))))
	if pair == nil {
		_, _ = unix.Close(int(fds[0])), unix.Close(int(fds[1]))
	}
	return []outcome{
		{"socketcall(SOCKET, AF_INET)", result(closeIfFD(call(1 /* SYS_SOCKET */, unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)))},
		{"socketcall(SOCKET, AF_UNIX)", result(closeIfFD(call(1, unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)))},
		{"socketcall(SOCKETPAIR)", result(pair)},
	}
}
