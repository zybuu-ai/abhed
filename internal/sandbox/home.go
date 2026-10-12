package sandbox

import (
	"os"
	"path/filepath"
)

// CommandHome is the HOME sb gives its commands, as a host path, and whether
// the host can read it; a sandbox that does not say is taken as hidden.
func CommandHome(sb Sandbox) (string, bool) {
	if h, ok := sb.(interface{ CommandHome() (string, bool) }); ok {
		return h.CommandHome()
	}
	return "", false
}

// HomeFunc is CommandHome as the bash tool and the policy take it.
func HomeFunc(sb Sandbox) func() (string, bool) {
	return func() (string, bool) { return CommandHome(sb) }
}

// hostHome is this process's HOME, which the process tier and none pass on.
func hostHome() (string, bool) {
	h := os.Getenv("HOME")
	return h, h != ""
}

// CommandHome is the host's HOME, which the process tier passes on.
func (s *Process) CommandHome() (string, bool) { return hostHome() }

// CommandHome is the host's HOME: none runs commands as they are.
func (n *None) CommandHome() (string, bool) { return hostHome() }

// CommandHome is the fence's private home folder, on the host.
func (f *Fence) CommandHome() (string, bool) {
	if f.tmp == "" {
		return "", false
	}
	return filepath.Join(f.tmp, "home"), true
}

// CommandHome is /tmp in the container, which the host cannot read.
func (c *Container) CommandHome() (string, bool) { return "/tmp", false }
