package sandbox

import (
	"context"
	"os/exec"
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
// outside the workspace, and a sandboxed shell should not depend on them.
var shellArgv = []string{"/bin/bash", "--noprofile", "--norc", "-i"}

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
