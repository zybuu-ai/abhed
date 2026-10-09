//go:build linux && !s390x

package seccomp

func addS390x(map[string]uint32) {}

// socketcallChecks: socket(2) is the only way to a socket here.
func socketcallChecks() []outcome { return nil }
