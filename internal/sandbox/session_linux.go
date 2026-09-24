//go:build linux

package sandbox

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	stopSignal = unix.SIGSTOP
	killSignal = unix.SIGKILL
)

// procStat returns the fields of /proc/<pid>/stat after the command name,
// which may itself hold spaces and parentheses: state is [0], the session
// [3], the start time [19].
func procStat(pid int) []string {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") // #nosec G304 -- a numbered /proc entry
	if err != nil {
		return nil
	}
	s := string(stat)
	return strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
}

// startTime reads when pid started, in clock ticks since boot. An exited but
// unreaped process still has it.
func startTime(pid int) (uint64, bool) {
	f := procStat(pid)
	if len(f) < 20 {
		return 0, false
	}
	t, err := strconv.ParseUint(f[19], 10, 64)
	return t, err == nil
}

// waitExited waits for pid, a child of this process, to exit, without
// reaping it: the pid stays taken until Wait.
func waitExited(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// sessionMembers lists the processes whose session is sid.
func sessionMembers(sid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	want := strconv.Itoa(sid)
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if f := procStat(pid); len(f) > 3 && f[3] == want {
			out = append(out, pid)
		}
	}
	return out
}

// signalMember signals pid only if it is in session sid. A pidfd names the
// process itself, so the check and the signal are about the same process
// even if the number is reused in between.
func signalMember(pid, sid int, sig unix.Signal) bool {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return false
	}
	defer func() { _ = unix.Close(fd) }()
	if f := procStat(pid); len(f) < 4 || f[3] != strconv.Itoa(sid) {
		return false
	}
	return unix.PidfdSendSignal(fd, sig, nil, 0) == nil
}
