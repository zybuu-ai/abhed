//go:build unix

package app

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

// runSSH replaces this process with the system ssh. Its ProxyCommand runs
// abhed again, which would refuse to run under an Abhed process as it does
// inside an agent's command; once replaced, there is none above it.
func runSSH(_ context.Context, args []string) (int, error) {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return 0, err
	}
	return 0, syscall.Exec(path, append([]string{"ssh"}, args...), os.Environ()) // #nosec G204 -- the person's own ssh, with options built for one sandbox
}
