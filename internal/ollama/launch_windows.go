//go:build windows

package ollama

import (
	"os"
	"os/exec"
	"syscall"
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

// termLaunched kills a server launch started in this process; Windows has
// no TERM for it. false when pid is not such a child or has exited.
func termLaunched(pid int) bool {
	c, ok := childOf(pid)
	if !ok || c.proc == nil {
		return false
	}
	return c.signal(c.proc.Kill) == nil
}

// killLaunched is termLaunched.
func killLaunched(pid int) bool { return termLaunched(pid) }
