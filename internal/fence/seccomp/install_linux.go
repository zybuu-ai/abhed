//go:build linux

package seccomp

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Install sets no_new_privs and loads p's filter on every thread of this
// process, for good. It is meant for a re-executed helper just before it
// execs the command, which inherits both.
func Install(p Policy) error {
	if _, ok := arches[runtime.GOARCH]; !ok {
		return ErrUnsupported
	}
	prog, err := p.Program(runtime.GOARCH)
	if err != nil {
		return err
	}
	filter := make([]unix.SockFilter, len(prog))
	for i, in := range prog {
		filter[i] = unix.SockFilter{Code: in.Code, Jt: in.Jt, Jf: in.Jf, K: in.K}
	}
	fprog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]} // #nosec G115 -- the assembler refuses more than 4096 instructions

	// no_new_privs and the filter must land on the same thread; TSYNC then
	// copies both to the others.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("seccomp: setting no_new_privs: %w", err)
	}
	r, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC,
		uintptr(unsafe.Pointer(&fprog))) // #nosec G103 -- the kernel ABI's struct, passed to the call it is for
	runtime.KeepAlive(filter)
	if errno != 0 {
		return fmt.Errorf("seccomp: loading the %s filter: %w", Profile, errno)
	}
	if r != 0 {
		return fmt.Errorf("seccomp: thread %d could not take the %s filter", r, Profile)
	}
	return nil
}
