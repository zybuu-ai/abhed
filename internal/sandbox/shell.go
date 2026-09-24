package sandbox

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Interactive is implemented by backends that can host a long-lived shell on
// a terminal, under the same confinement as Command. A person at the
// workbench gets one per terminal tab.
type Interactive interface {
	Shell(ctx context.Context, cwd string) *exec.Cmd
	// Backend names the mechanism, for a person reading a terminal banner.
	Backend() string
}

// shellArgv is bash without the operator's profile or rc files: they live
// outside the workspace, and a sandboxed shell should not depend on them. It
// is a login shell with huponexit, so jobs still running when it exits are
// hung up rather than left behind.
var shellArgv = []string{"/bin/bash", "--noprofile", "--norc", "-l", "-O", "huponexit", "-i"}

// hangUpDelay is how long a shell has to pass a hang-up on to its jobs
// before it is killed.
const hangUpDelay = 2 * time.Second

// hangUp ends a shell the way closing a terminal does. An interactive bash
// puts each background job in a process group of its own, so killing the
// shell leaves them running; on SIGHUP it hangs them up too. Jobs that left
// its job table, `( cmd & )`, or ignore the hang-up are ended by Leader.Wait.
func hangUp(cmd *exec.Cmd) *exec.Cmd {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGHUP) }
	cmd.WaitDelay = hangUpDelay
	return cmd
}

// shellEnv names the shell and its tier in the prompt, keeps history in
// memory only, and silences macOS's notice about the default shell.
func shellEnv(t Tier) []string {
	ps1 := `\[\e[2m\](sandbox: ` + string(t) + `)\[\e[0m\] \[\e[36m\]\W\[\e[0m\] \$ `
	if t == TierNone {
		ps1 = `\[\e[1;31m\](no sandbox)\[\e[0m\] \[\e[36m\]\W\[\e[0m\] \$ `
	}
	return []string{
		"TERM=xterm-256color",
		"PS1=" + ps1,
		"HISTFILE=",
		"BASH_SILENCE_DEPRECATION_WARNING=1",
		"ABHED_TERMINAL=1",
	}
}

// hostEnv is the server's environment without its own settings, which hold
// database URLs and keys a person at the terminal has no use for.
func hostEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ABHED_") {
			out = append(out, kv)
		}
	}
	return out
}
