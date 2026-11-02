//go:build !windows

package ollama

import (
	"os"
	"os/exec"
	"syscall"
)

// launch starts path detached: its own process group, so a Ctrl-C at the
// person's terminal does not reach it, no input, output to log. It is
// reaped when it exits, which exited reports, and is not stopped when
// Abhed exits.
func launch(path string, args, env []string, log *os.File) (int, <-chan error, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if log != nil {
		cmd.Stdout, cmd.Stderr = log, log
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return pid, exited, nil
}
