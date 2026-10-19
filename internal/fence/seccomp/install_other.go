//go:build !linux

package seccomp

// Install returns ErrUnsupported: there is no seccomp off Linux.
func Install(Policy) error { return ErrUnsupported }

// InstallNotify returns ErrUnsupported, as Install does.
func InstallNotify(Policy) (int, error) { return -1, ErrUnsupported }

// Serve returns ErrUnsupported: there is no listener off Linux.
func Serve(int, <-chan struct{}, func(string)) error { return ErrUnsupported }
