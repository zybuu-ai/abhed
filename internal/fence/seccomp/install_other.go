//go:build !linux

package seccomp

// Install returns ErrUnsupported: there is no seccomp off Linux.
func Install(Policy) error { return ErrUnsupported }
