package ocsf

import "time"

// SetSinkQueue sets the sink's queue length for a test.
func SetSinkQueue(n int) (restore func()) {
	old := sinkQueue
	sinkQueue = n
	return func() { sinkQueue = old }
}

// Backoff is the wait a failed write set, for a test.
func (s *Sink) Backoff() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backoff
}

// MinBackoff is the first wait after a failure.
func MinBackoff() time.Duration { return minBackoff }
