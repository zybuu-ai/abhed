//go:build !linux && !darwin

package sandbox

// EndSession cannot find a session's processes on this platform.
func EndSession(int) {}
