//go:build windows

package ollama

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// launch starts path detached in a process group of its own, no input,
// output to log. It is kept as this process's own child.
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
	c := addChild(pid)
	c.proc = cmd.Process
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		c.exit()
		exited <- err
	}()
	return pid, exited, nil
}

// stopLaunched kills a server launch started in this process.
func stopLaunched(pid int, grace time.Duration) (bool, error) {
	c, ok := childOf(pid)
	if !ok || c.proc == nil {
		return false, nil
	}
	if err := c.signal(c.proc.Kill); err != nil {
		return false, err
	}
	select {
	case <-c.done:
	case <-time.After(grace):
	}
	return true, nil
}
