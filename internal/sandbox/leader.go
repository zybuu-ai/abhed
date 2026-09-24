package sandbox

import "os/exec"

// A shell started on a terminal leads a session of its own, and what it left
// running (a job out of its table, one ignoring the hang-up, one that keeps
// forking) is still in that session when it exits. Ending those must never
// touch a process this server did not start, so the sweep runs only while the
// exited shell is unreaped: its pid, and so its session id, cannot be taken
// by anything else until Wait reaps it. The leader's start time is checked
// before each sweep as well.

// maxSweepPasses bounds the passes that stop a session's processes. Each
// pass stops what it finds, so a job that forks is caught within a few.
const maxSweepPasses = 50

// Leader is a session leader this process started: its pid and when it
// started, which together name it even if the pid is later reused.
type Leader struct {
	Pid   int
	start uint64
	ok    bool
}

// Lead names the session leader cmd started. Call it right after Start, on a
// command started with its own session, as a terminal starts one.
func Lead(cmd *exec.Cmd) Leader {
	if cmd.Process == nil {
		return Leader{}
	}
	pid := cmd.Process.Pid
	start, ok := startTime(pid)
	return Leader{Pid: pid, start: start, ok: ok}
}

// Wait waits for the shell to exit, ends every process left in its session
// while the unreaped shell still holds the session id, and then reaps it.
// refused says why the session was not swept, for the operator's log; it is
// empty when the sweep ran.
func (l Leader) Wait(cmd *exec.Cmd) (refused string, err error) {
	switch {
	case !l.ok:
		refused = "the shell's start time could not be read when it started"
	default:
		if werr := waitExited(l.Pid); werr != nil {
			refused = "waiting for the shell to exit failed: " + werr.Error()
		} else {
			refused = l.sweep()
		}
	}
	return refused, cmd.Wait()
}

// sweep stops every process in the leader's session, pass after pass until a
// pass finds none it has not stopped, then kills them, and kills anything
// that appears after. It does nothing unless the process at the leader's pid
// is the one Lead named, and says why when it does nothing.
func (l Leader) sweep() string {
	if !l.ok || l.Pid <= 1 {
		return "the shell was never named"
	}
	if ok, why := sweepSupported(); !ok {
		return why
	}
	if start, ok := startTime(l.Pid); !ok || start != l.start {
		return "the process at the shell's pid is not the shell that started there"
	}
	stopped := map[int]bool{}
	for range maxSweepPasses {
		fresh := 0
		for _, pid := range sessionMembers(l.Pid) {
			if pid == l.Pid || stopped[pid] {
				continue
			}
			if signalMember(pid, l.Pid, stopSignal) {
				stopped[pid] = true
				fresh++
			}
		}
		if fresh == 0 {
			break
		}
	}
	for pid := range stopped {
		signalMember(pid, l.Pid, killSignal)
	}
	// A stopped group can be continued by the kernel when a member exits, and
	// a process continued in that instant may fork once more.
	for range maxSweepPasses {
		left := 0
		for _, pid := range sessionMembers(l.Pid) {
			if pid != l.Pid && signalMember(pid, l.Pid, killSignal) {
				left++
			}
		}
		if left == 0 {
			break
		}
	}
	return ""
}
