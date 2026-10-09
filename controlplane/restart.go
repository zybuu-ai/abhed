package controlplane

import "time"

// MaxBackoff bounds the delay between restarts.
const MaxBackoff = time.Hour

// StableAfter is how long a sandbox must stay Ready for its restart count
// to start again from zero.
const StableAfter = 10 * time.Minute

// RestartDue reports whether the restart policy starts a sandbox that has
// ended in state: on-failure restarts Error, always restarts Error and
// Completed, and never restarts nothing. A sandbox a client stopped is
// never restarted.
func RestartDue(policy string, state State) bool {
	switch policy {
	case RestartAlways:
		return state == StateError || state == StateCompleted
	case RestartOnFailure:
		return state == StateError
	}
	return false
}

// Backoff is the delay before restart number n+1, given n made already:
// base doubled n times, at most MaxBackoff.
func Backoff(base time.Duration, n int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	d := base
	for range n {
		if d >= MaxBackoff/2 {
			return MaxBackoff
		}
		d *= 2
	}
	return min(d, MaxBackoff)
}
