//go:build windows

package ollama

import (
	"os"
	"os/exec"
	"syscall"
)

// launch starts path detached in a process group of its own, no input,
// output to log. It is not stopped when Abhed exits.
func launch(path string, args, env []string, log *os.File) (int, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
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
