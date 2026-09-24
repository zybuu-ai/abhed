//go:build darwin

package sandbox

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// EndSession kills every process still in session sid: what a shell started
// and left behind, however it was detached from the shell's job table. A
// process that made a session of its own (setsid) is not in it.
func EndSession(sid int) {
	if sid <= 1 {
		return
	}
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return
	}
	for _, p := range procs {
		pid := int(p.Proc.P_pid)
		if pid <= 1 || pid == os.Getpid() {
			continue
		}
		if s, err := unix.Getsid(pid); err == nil && s == sid {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
