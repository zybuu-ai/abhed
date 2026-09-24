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
func (l Leader) Wait(cmd *exec.Cmd) error {
	if l.ok && waitExited(l.Pid) == nil {
		l.sweep()
	}
	return cmd.Wait()
}

// sweep stops every process in the leader's session, pass after pass until a
// pass finds none it has not stopped, then kills them. It does nothing unless
// the process at the leader's pid is the one Lead named. It reports whether
// it ran.
func (l Leader) sweep() bool {
	if !l.ok || l.Pid <= 1 {
		return false
	}
	if start, ok := startTime(l.Pid); !ok || start != l.start {
		return false
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
	return true
}
