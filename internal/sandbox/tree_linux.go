//go:build linux

package sandbox

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// processParents maps each live process to its parent.
func processParents() map[int]int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make(map[int]int, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		// A zombie has already ended; it is only waiting to be reaped.
		if f := procStat(pid); len(f) > 1 && f[0] != "Z" {
			if ppid, err := strconv.Atoi(f[1]); err == nil {
				out[pid] = ppid
			}
		}
	}
	return out
}

// userProcesses counts the tasks (threads) of this real user, as the kernel
// counts them against RLIMIT_NPROC.
func userProcesses() (int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	uid := strconv.Itoa(os.Getuid())
	total := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		f, err := os.Open("/proc/" + e.Name() + "/status") // #nosec G304 -- a numbered /proc entry
		if err != nil {
			continue
		}
		mine, threads := false, 0
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, _ := strings.Cut(sc.Text(), ":")
			switch fields := strings.Fields(v); {
			case k == "Uid" && len(fields) > 0:
				mine = fields[0] == uid
			case k == "Threads" && len(fields) > 0:
				threads, _ = strconv.Atoi(fields[0])
			}
		}
		_ = f.Close()
		if mine {
			total += threads
		}
	}
	return total, total > 0
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

// treeMember is a stopped descendant, held by a pidfd, which names the
// process itself even if its number is reused.
type treeMember struct{ fd int }

// stopMember stops pid through a pidfd if its parent, read after the pidfd
// names the process, is in tree.
func stopMember(pid int, tree map[int]bool) (treeMember, bool) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return treeMember{}, false
	}
	f := procStat(pid)
	ppid := -1
	if len(f) > 1 {
		ppid, _ = strconv.Atoi(f[1])
	}
	if !tree[ppid] || unix.PidfdSendSignal(fd, stopSignal, nil, 0) != nil {
		_ = unix.Close(fd)
		return treeMember{}, false
	}
	return treeMember{fd: fd}, true
}

func (m treeMember) kill() {
	_ = unix.PidfdSendSignal(m.fd, killSignal, nil, 0)
	_ = unix.Close(m.fd)
}
