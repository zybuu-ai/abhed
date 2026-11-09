//go:build darwin

package termline

import "golang.org/x/sys/unix"

const getTermios = unix.TIOCGETA

// Process is pid's command name as the kernel keeps it, its parent and its
// process group; false if no live process has that number.
func Process(pid int) (name string, ppid, pgid int, ok bool) {
	if pid <= 0 {
		return "", 0, 0, false
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	// A zombie (SZOMB) has already ended.
	if err != nil || int(p.Proc.P_pid) != pid || p.Proc.P_stat == 5 {
		return "", 0, 0, false
	}
	return unix.ByteSliceToString(p.Proc.P_comm[:]), int(p.Eproc.Ppid), int(p.Eproc.Pgid), true
}
