//go:build !unix

package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

// runSSH runs the system ssh on this terminal and returns its exit status.
func runSSH(ctx context.Context, args []string) (int, error) {
	cmd := exec.CommandContext(ctx, "ssh", args...) // #nosec G204 -- the person's own ssh, with options built for one sandbox
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	if ee := new(exec.ExitError); errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}
