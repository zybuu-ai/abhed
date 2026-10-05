//go:build darwin

package sandbox

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// processParents maps each live process to its parent.
func processParents() map[int]int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	out := make(map[int]int, len(procs))
	for _, p := range procs {
		// A zombie (SZOMB) has already ended; it is only waiting to be reaped.
		if pid := int(p.Proc.P_pid); pid > 1 && pid != os.Getpid() && p.Proc.P_stat != 5 {
			out[pid] = int(p.Eproc.Ppid)
		}
	}
	return out
}

// userProcesses counts the processes of this real user, as the kernel counts
// them against RLIMIT_NPROC.
func userProcesses() (int, bool) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.ruid", os.Getuid())
	if err != nil {
		return 0, false
	}
	return len(procs), true
}

// procSoftLimit is this process's RLIMIT_NPROC in force, which a command's
// limit may not exceed; replaced in tests.
var procSoftLimit = func() (uint64, bool) {
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NPROC, &rl); err != nil {
		return 0, false
	}
	return rl.Cur, true
}

// treeMember is a stopped descendant, signalled by number: once it and its
// parent are stopped, nothing can reap it, so the number stays its own.
type treeMember struct{ pid int }

// stopMember stops pid if its parent is in tree. macOS has no pidfd, so the
// parent is read again once the stop has taken effect, and a process that
// turned out not to be in the tree is continued and left alone. In that window
// a reused pid's process is briefly stopped, and one already stopped (Ctrl-Z)
// is continued; the same window signalMember accepts.
func stopMember(pid int, tree map[int]bool) (treeMember, bool) {
	if syscall.Kill(pid, stopSignal) != nil {
		return treeMember{}, false
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(p.Proc.P_pid) != pid || !tree[int(p.Eproc.Ppid)] {
		_ = syscall.Kill(pid, syscall.SIGCONT)
		return treeMember{}, false
	}
	return treeMember{pid: pid}, true
}

func (m treeMember) kill() { _ = syscall.Kill(m.pid, killSignal) }

// ancestorNames lists the command names of this process's ancestors, nearest
// first, as the kernel keeps them (at most 16 bytes).
func ancestorNames() []string {
	var out []string
	pid := os.Getppid()
	for i := 0; pid > 1 && i < maxAncestors; i++ {
		p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil || int(p.Proc.P_pid) != pid {
			break
		}
		out = append(out, unix.ByteSliceToString(p.Proc.P_comm[:]))
		pid = int(p.Eproc.Ppid)
	}
	return out
}

// marked lists this user's processes, other than this one, whose arguments or
// environment hold marker followed by its terminating NUL.
func marked(marker string) []int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.ruid", os.Getuid())
	if err != nil {
		return nil
	}
	want := []byte(marker + "\x00")
	var out []int
	for _, p := range procs {
		pid := int(p.Proc.P_pid)
		if pid <= 1 || pid == os.Getpid() || p.Proc.P_stat == 5 {
			continue
		}
		if args, ok := procArgs(p.Proc.P_pid); ok && bytes.Contains(args, want) {
			out = append(out, pid)
		}
	}
	return out
}

// kernProcArgs2 is KERN_PROCARGS2 in <sys/sysctl.h>, which x/sys does not name.
const kernProcArgs2 = 49

// procArgs reads pid's arguments and environment (KERN_PROCARGS2) into a
// buffer of kern.argmax: asked for its size first, as SysctlRaw does, the
// kernel answers with less than another process's whole environment.
func procArgs(pid int32) ([]byte, bool) {
	max, err := unix.SysctlUint32("kern.argmax")
	if err != nil || max == 0 {
		return nil, false
	}
	buf := make([]byte, max)
	size := uintptr(max)
	mib := [3]int32{unix.CTL_KERN, kernProcArgs2, pid}
	// #nosec G103 -- the sysctl(2) call needs the buffer's address
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), 3,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, 0)
	if errno != 0 || size > uintptr(max) {
		return nil, false
	}
	return buf[:size], true
}
