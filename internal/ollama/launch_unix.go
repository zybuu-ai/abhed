//go:build !windows

package ollama

import (
	"os"
	"os/exec"
	"syscall"
)

// launch starts path detached: its own process group, so a Ctrl-C at the
// person's terminal does not reach it, no input, output to log. It is
// reaped when it exits, which exited reports. It is kept as this process's
// own child, so stopLaunched can stop it and nothing else.
func launch(path string, args, env []string, log *os.File) (int, <-chan error, error) {
	cmd := exec.Command(path, args...) // #nosec G204 -- the ollama program found on PATH, started after the person agrees
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if log != nil {
		cmd.Stdout, cmd.Stderr = log, log
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	pid := cmd.Process.Pid
	c := addChild(pid)
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		c.exit()
		exited <- err
	}()
	return pid, exited, nil
}

// signalGroup sends sig to the process group pid leads. The group is the one
// launch made for a child of this process; childOf checked it has not been
// reaped, so the id cannot have been given to another process.
func signalGroup(pid int, sig syscall.Signal) error { return syscall.Kill(-pid, sig) }

// termLaunched asks a server launch started in this process to stop: TERM to
// its process group. false when pid is not such a child or has exited.
func termLaunched(pid int) bool { return signalLaunched(pid, syscall.SIGTERM) }

// killLaunched kills such a server's process group.
func killLaunched(pid int) bool { return signalLaunched(pid, syscall.SIGKILL) }

func signalLaunched(pid int, sig syscall.Signal) bool {
	c, ok := childOf(pid)
	if !ok {
		return false
	}
	return c.signal(func() error { return signalGroup(pid, sig) }) == nil
}
