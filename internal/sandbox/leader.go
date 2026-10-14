package sandbox

import (
	"fmt"
	"os/exec"
	"time"
)

// Leftovers are swept only while the exited shell is unreaped, so its session
// id cannot be reused: never a process this server did not start.

// The sweep's view of the system, replaced in tests.
var (
	listMembers = sessionMembers
	signalOne   = signalMember
	startOf     = startTime
	canSweep    = sweepSupported
	// cannotSignal reports a process this user may not signal (EPERM).
	cannotSignal = signalRefused
)

// maxSweepPasses bounds the passes that stop a session's processes. Each
// pass stops what it finds, so a job that forks is caught within a few.
const maxSweepPasses = 50

// sweepGrace bounds the wait for killed processes to go: a CPU-starved one
// stays listed longer than any fixed number of passes.
var sweepGrace = 10 * time.Second

// Leader is a session leader this process started: its pid and when it
// started, which together name it even if the pid is later reused.
type Leader struct {
	Pid   int
	start uint64
	ok    bool
	// marker is what every process the shell started carries, or "".
	marker string
}

// Lead names the session leader cmd started. Call it right after Start, on a
// command started with its own session, as a terminal starts one.
func Lead(cmd *exec.Cmd) Leader {
	if cmd.Process == nil {
		return Leader{}
	}
	pid := cmd.Process.Pid
	start, ok := startTime(pid)
	return Leader{Pid: pid, start: start, ok: ok, marker: markerIn(cmd.Env)}
}

// Wait waits for the shell to exit, ends every process left in its session
// while the unreaped shell still holds the session id, and then reaps it.
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
	// A process that left the session (setsid) still carries the marker.
	if l.marker != "" {
		endMarked(l.marker)
	}
	return refused, cmd.Wait()
}

// sweep stops then kills every process in the leader's session, and does
// nothing unless the leader's pid is still the one Lead named.
func (l Leader) sweep() string {
	if !l.ok || l.Pid <= 1 {
		return "the shell was never named"
	}
	if ok, why := canSweep(); !ok {
		return why
	}
	if start, ok := startOf(l.Pid); !ok || start != l.start {
		return "the process at the shell's pid is not the shell that started there"
	}
	stopped := map[int]bool{}
	for range maxSweepPasses {
		fresh := 0
		for _, pid := range listMembers(l.Pid) {
			if pid == l.Pid || stopped[pid] {
				continue
			}
			if signalOne(pid, l.Pid, stopSignal) {
				stopped[pid] = true
				fresh++
			}
		}
		if fresh == 0 {
			break
		}
	}
	killed := map[int]bool{}
	for pid := range stopped {
		if signalOne(pid, l.Pid, killSignal) {
			killed[pid] = true
		}
	}
	// A stopped group may resume and fork as a member exits, so every pass
	// kills all members until none is left.
	deadline := time.Now().Add(sweepGrace)
	wait := sweepWaitMin
	for {
		left, fresh, unsignalled := 0, 0, 0
		for _, pid := range listMembers(l.Pid) {
			if pid == l.Pid {
				continue
			}
			if !signalOne(pid, l.Pid, killSignal) {
				// Gone, or no longer in the session, is not counted: only a refusal.
				if cannotSignal(pid) {
					unsignalled++
				}
				continue
			}
			left++
			if !killed[pid] {
				killed[pid] = true
				fresh++
			}
		}
		if left == 0 {
			// Listed but not signalled: another user's, a setuid program's, say.
			if unsignalled > 0 {
				return fmt.Sprintf("%d processes in the shell's session could not be signalled (another user's, or one that changed its user) and may still run", unsignalled)
			}
			return ""
		}
		if time.Now().After(deadline) {
			return "processes were still running in the shell's session after it was swept"
		}
		if fresh > 0 {
			wait = sweepWaitMin
			continue
		}
		time.Sleep(min(wait, time.Until(deadline)+time.Millisecond))
		wait = min(2*wait, sweepWaitMax)
	}
}

// sweepWaitMin and sweepWaitMax bound the wait between passes of a sweep that
// find only members it has already killed.
const sweepWaitMin = time.Millisecond

var sweepWaitMax = 50 * time.Millisecond
