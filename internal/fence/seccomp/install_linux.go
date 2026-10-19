//go:build linux

package seccomp

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Install sets no_new_privs and loads p's filter on every thread of this
// process, for good. The command exec'd next inherits both.
func Install(p Policy) error {
	p.Notify = false
	_, err := install(p, unix.SECCOMP_FILTER_FLAG_TSYNC)
	return err
}

// InstallNotify is Install with p's refusals sent to a listener, whose
// descriptor it returns (close-on-exec) for Abhed to answer them. It needs
// Linux 5.7 or later; where the kernel refuses the flags nothing is
// installed, and the error wraps ErrNotifyUnsupported.
func InstallNotify(p Policy) (int, error) {
	p.Notify = true
	fd, err := install(p, unix.SECCOMP_FILTER_FLAG_TSYNC|unix.SECCOMP_FILTER_FLAG_TSYNC_ESRCH|unix.SECCOMP_FILTER_FLAG_NEW_LISTENER)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return -1, fmt.Errorf("%w: %w", ErrNotifyUnsupported, err)
	}
	return fd, err
}

func install(p Policy, flags uintptr) (int, error) {
	if _, ok := arches[runtime.GOARCH]; !ok {
		return -1, ErrUnsupported
	}
	prog, err := p.Program(runtime.GOARCH)
	if err != nil {
		return -1, err
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
		return -1, fmt.Errorf("seccomp: setting no_new_privs: %w", err)
	}
	r, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, flags,
		uintptr(unsafe.Pointer(&fprog))) // #nosec G103 -- the kernel ABI's struct, passed to the call it is for
	runtime.KeepAlive(filter)
	if errno != 0 {
		return -1, fmt.Errorf("seccomp: loading the %s filter: %w", Profile, errno)
	}
	if flags&unix.SECCOMP_FILTER_FLAG_NEW_LISTENER != 0 {
		return int(r), nil // the listener; with TSYNC_ESRCH a thread that could not sync fails the call instead
	}
	if r != 0 {
		return -1, fmt.Errorf("seccomp: thread %d could not take the %s filter", r, Profile)
	}
	return -1, nil
}

// notif is struct seccomp_notif, and notifResp struct seccomp_notif_resp.
type notif struct {
	ID    uint64
	PID   uint32
	Flags uint32
	Nr    int32
	Arch  uint32
	IP    uint64
	Args  [6]uint64
}

type notifResp struct {
	ID    uint64
	Val   int64
	Error int32
	Flags uint32
}

// Serve answers every refusal sent to the listener fd with EPERM, telling
// refused the call's name, until stop is closed or no process is left under
// the filter. It never lets a call run: it sends no CONTINUE.
func Serve(fd int, stop <-chan struct{}, refused func(name string)) error {
	names := callNames(runtime.GOARCH)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} // #nosec G115 -- a descriptor fits
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		fds[0].Revents = 0
		n, err := unix.Poll(fds, 100)
		if errors.Is(err, unix.EINTR) || n == 0 {
			continue
		}
		if err != nil {
			return err
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			return nil // POLLHUP: the filter's last process is gone
		}
		var req notif
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SECCOMP_IOCTL_NOTIF_RECV, uintptr(unsafe.Pointer(&req))); errno != 0 { // #nosec G103 -- the kernel ABI's struct
			if errno == unix.ENOENT || errno == unix.EINTR {
				continue // the caller went away before it was read
			}
			return fmt.Errorf("seccomp: reading a refusal: %w", errno)
		}
		resp := notifResp{ID: req.ID, Error: -int32(unix.EPERM)}
		// ENOENT: the caller is gone, and its call with it.
		_, _, _ = unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SECCOMP_IOCTL_NOTIF_SEND, uintptr(unsafe.Pointer(&resp))) // #nosec G103 -- the kernel ABI's struct
		name, ok := names[uint32(req.Nr)]                                                                                  // #nosec G115 -- a call number
		if !ok {
			name = fmt.Sprintf("call %d", req.Nr)
		}
		refused(refusalName(name, req.Args))
	}
}
