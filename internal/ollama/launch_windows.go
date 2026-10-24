//go:build windows

package ollama

import (
	"os"
	"os/exec"
	"syscall"
)

// launch starts path detached in a process group of its own, no input,
// output to log. It is not stopped when Abhed exits.
func launch(path string, args, env []string, log *os.File) (int, <-chan error, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
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
