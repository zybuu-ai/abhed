package ocsf

// SetSinkQueue sets the sink's queue length for a test.
func SetSinkQueue(n int) (restore func()) {
	old := sinkQueue
	sinkQueue = n
	return func() { sinkQueue = old }
}
