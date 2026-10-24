//go:build !windows

package ollama

import (
	"os"
	"os/exec"
	"syscall"
)

// launch starts path detached: its own process group, so a Ctrl-C at the
// person's terminal does not reach it, no input, output to log. It is
// reaped when it exits and is not stopped when Abhed exits.
func launch(path string, args, env []string, log *os.File) (int, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if log != nil {
		cmd.Stdout, cmd.Stderr = log, log
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, nil
}
