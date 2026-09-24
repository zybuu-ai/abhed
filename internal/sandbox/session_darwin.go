//go:build darwin

package sandbox

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	stopSignal = syscall.SIGSTOP
	killSignal = syscall.SIGKILL
)

// startTime reads when pid started, from the process table, which keeps an
// exited but unreaped process.
func startTime(pid int) (uint64, bool) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(p.Proc.P_pid) != pid {
		return 0, false
	}
	t := p.Proc.P_starttime
	return uint64(t.Sec)*1_000_000 + uint64(t.Usec), true // #nosec G115 -- a time since 1970 is positive
}

// waitExited waits for pid, a child of this process, to exit, without
// reaping it: the pid stays taken until Wait.
func waitExited(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(kq) }()
	ev := []unix.Kevent_t{{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}} // #nosec G115 -- a pid is positive
	if _, err := unix.Kevent(kq, ev, nil, nil); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil // it has exited already, and is still unreaped
		}
		return err
	}
	out := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, nil, out, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
}

// sessionMembers lists the processes whose session is sid.
func sessionMembers(sid int) []int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	var out []int
	for _, p := range procs {
		pid := int(p.Proc.P_pid)
		// A zombie (SZOMB) has already ended; it is only waiting to be reaped.
		if pid <= 1 || pid == os.Getpid() || p.Proc.P_stat == 5 {
			continue
		}
		if s, err := unix.Getsid(pid); err == nil && s == sid {
			out = append(out, pid)
		}
	}
	return out
}

// sweepSupported: macOS has what the sweep needs.
func sweepSupported() (bool, string) { return true, "" }

// signalMember signals pid only if it is in session sid. macOS has no pidfd,
// so a stop is checked again once it has taken effect, when the process can
// no longer exit on its own, and undone if the pid turned out to be another's.
func signalMember(pid, sid int, sig syscall.Signal) bool {
	if s, err := unix.Getsid(pid); err != nil || s != sid {
		return false
	}
	if syscall.Kill(pid, sig) != nil {
		return false
	}
	if sig == stopSignal {
		if s, err := unix.Getsid(pid); err != nil || s != sid {
			_ = syscall.Kill(pid, syscall.SIGCONT)
			return false
		}
	}
	return true
}
