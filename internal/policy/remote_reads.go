package policy

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// AllowRemoteReads declares the commands an ssh call may run as a read
// (ssh.read_commands): an exact command or a pattern for one simple
// command, such as `uptime` or `systemctl status *`. A matching call is
// read-only, so plan mode allows it and no mode asks for it; deny and ask
// rules and the destructive check still come first.
func (e *Engine) AllowRemoteReads(patterns ...string) error {
	for _, p := range patterns {
		r, err := ParseRemoteRead(p)
		if err != nil {
			return err
		}
		e.RemoteReads = append(e.RemoteReads, r)
	}
	return nil
}

// ParseRemoteRead parses one ssh.read_commands entry. One that would run
// more than one simple command, or that the shell reads differently than
// written, is refused: it could never be a single read.
func ParseRemoteRead(p string) (Rule, error) {
	s := strings.TrimSpace(p)
	if s == "" {
		return Rule{}, fmt.Errorf("ssh.read_commands: an entry is empty")
	}
	canon := tools.CanonicalCommand(s)
	if hasShellControl(s) || canon.Reworded || canon.Unparsed() || canon.Incomplete() {
		return Rule{}, fmt.Errorf("ssh.read_commands: %q is not one simple command "+
			"(write an exact command or a pattern such as \"systemctl status *\"; no ; & | < > ( ) ` ${ or newline)", p)
	}
	// The program is named literally: a wildcard there would make any program a read.
	if program := strings.Fields(s)[0]; strings.ContainsAny(program, "*?$") || isAssignment(program) {
		return Rule{}, fmt.Errorf("ssh.read_commands: %q must start with the program it runs, written out (such as \"df -h\")", p)
	}
	name := rawProgram(s)
	spec, known := knownReads[name]
	words, _, _ := shellWords(s)
	wildcard := strings.ContainsAny(s, "*?")
	switch {
	// A wildcard is accepted only for a program on the read-only list,
	// called as that list says; any other is declared exactly.
	case wildcard && !known:
		return Rule{}, fmt.Errorf("ssh.read_commands: %q has a wildcard, and %s is not on the list of programs a wildcard may name; declare the exact command instead", p, name)
	case wildcard && !readCallOK(spec, words[1:], func(w string) bool { return strings.ContainsAny(w, "*?") }):
		return Rule{}, fmt.Errorf("ssh.read_commands: %q is not a read form of %s (see docs/ops/infrastructure.md#ssh); declare the exact command instead", p, name)
	case !wildcard && known && !readCallOK(spec, words[1:], func(string) bool { return false }):
		return Rule{}, fmt.Errorf("ssh.read_commands: %q is not a read form of %s: it uses a verb or option the list does not hold as a read (see docs/ops/infrastructure.md#ssh)", p, name)
	case !wildcard && !known && runsCommandsIn(s):
		return Rule{}, fmt.Errorf("ssh.read_commands: %q runs %s, which runs what it is given, so it is not a read", p, name)
	}
	if what, destructive := canon.Destructive(); destructive {
		return Rule{}, fmt.Errorf("ssh.read_commands: %q is not a read (%s)", p, what)
	}
	// A call that runs another command or writes a file is never a read.
	if len(canon.Commands()) > 1 || len(canon.Writes()) > 0 || canon.WriteHidden() != "" {
		return Rule{}, fmt.Errorf("ssh.read_commands: %q runs another command or writes a file, so no call is a read by it", p)
	}
	re, err := globToRegexp(s)
	if err != nil {
		return Rule{}, fmt.Errorf("ssh.read_commands: %q: %w", p, err)
	}
	return Rule{raw: s, tool: "ssh", pattern: re, glob: s}, nil
}

// remoteReadRule is the ssh.read_commands entry the call's command matches,
// or "": only one simple command, whose words the text shows, that no
// check reads as destructive.
func (e *Engine) remoteReadRule(rr remoteRead) string {
	if len(e.RemoteReads) == 0 || !rr.narrow || !rr.complete || rr.canon.Hidden || rr.canon.IFSSplit {
		return ""
	}
	// A secret on stdin may come back in what the command prints, transformed
	// past the mask, so it is never a read; nor is a command the parser finds
	// another inside (sh -c, eval, trap) or that writes a file (find -fprint).
	if rr.stdin || len(rr.canon.Commands()) > 1 || len(rr.canon.Writes()) > 0 || rr.canon.WriteHidden() != "" {
		return ""
	}
	if _, destructive := rr.canon.DestructiveWith(e.GitExtensions); destructive {
		return ""
	}
	if rr.canon.GitRunsOrWrites() != "" {
		return ""
	}
	text := rr.command
	if len(rr.allow) == 1 {
		text = strings.TrimPrefix(rr.allow[0], rr.host+"/")
	}
	// An exact entry matches only its own text. A wildcard one matches a call
	// to its program, on the read-only list, in a read form of it, with no
	// runner in front.
	forms, whole := strippedForms(text)
	plain := whole && len(forms) == 1 && forms[0] == strings.Join(strings.Fields(text), " ")
	for _, r := range e.RemoteReads {
		if !r.pattern.MatchString(text) {
			continue
		}
		if !strings.ContainsAny(r.glob, "*?") || plain && wildcardReadOK(text) {
			return r.raw
		}
	}
	return ""
}

// wildcardReadOK reports a call a wildcard entry may approve: its program
// on the read-only list, called in a form the list allows.
func wildcardReadOK(text string) bool {
	spec, known := knownReads[rawProgram(text)]
	if !known {
		return false
	}
	words, dollar, glob := shellWords(text)
	if len(words) == 0 || spec.strict && (dollar || glob) {
		return false
	}
	return readCallOK(spec, words[1:], func(string) bool { return false })
}

// carriesStdinSecret reports an ssh call that names a stored secret for its stdin.
func carriesStdinSecret(args json.RawMessage) bool {
	m, err := tools.DecodeArgs(args)
	if err != nil {
		return true // unreadable: taken as carrying one
	}
	v, found := tools.Lookup(m, "stdin_secret")
	s, isStr := v.(string)
	return found && v != nil && (!isStr || s != "")
}

// rawProgram is the program a command names first, by its base name.
func rawProgram(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return tools.CommandName(path.Base(fields[0]))
}

// programName is rawProgram with a version suffix such as python3.12's dropped.
func programName(command string) string {
	name := rawProgram(command)
	if trimmed := strings.TrimRight(name, "0123456789."); trimmed != "" {
		return trimmed
	}
	return name
}

// runsCommandsIn reports a command whose program runs what it is given, by
// its name as written (sqlite3) or with its version dropped (python3.12).
func runsCommandsIn(command string) bool {
	return runsCommands(rawProgram(command)) || runsCommands(programName(command))
}

// runsCommands reports a program that runs a command or code it is given:
// a shell, an interpreter, or a runner such as sudo, su or systemd-run.
func runsCommands(name string) bool {
	_, wrapper := wrapperWords[name]
	// The dynamic loader runs the program it is given: ld.so, ld-linux*.so.
	loader := strings.HasPrefix(name, "ld") && strings.Contains(name, ".so")
	return wrapper || loader || commandRunners[name]
}

// commandRunners are the programs beside wrapperWords that run what they
// are given as a command or as code.
var commandRunners = map[string]bool{
	// Shells.
	"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true, "ksh93": true, "mksh": true, "pdksh": true,
	"ash": true, "csh": true, "tcsh": true, "fish": true, "rc": true, "es": true, "yash": true, "busybox": true, "toybox": true,
	"source": true, ".": true, "eval": true, "exec": true, "command": true, "builtin": true,
	// Runners.
	"su": true, "runuser": true, "sudo": true, "doas": true, "pkexec": true, "sg": true, "newgrp": true,
	"chroot": true, "flock": true, "systemd-run": true, "systemd-nspawn": true, "machinectl": true, "script": true,
	"unshare": true, "nsenter": true, "taskset": true, "chrt": true, "numactl": true, "cgexec": true, "firejail": true,
	"strace": true, "ltrace": true, "gdb": true, "perf": true, "valgrind": true, "at": true, "batch": true,
	"tmux": true, "screen": true, "watch": true, "timeout": true, "nohup": true, "setsid": true, "nice": true,
	"ionice": true, "stdbuf": true, "env": true, "time": true, "xargs": true, "parallel": true, "ssh": true,
	"docker": true, "podman": true, "kubectl": true, "crictl": true, "lxc-attach": true, "expect": true, "dbus-run-session": true,
	// Editors and tools with an option or command that runs a program.
	"sed": true, "gsed": true, "ed": true, "ex": true, "vi": true, "vim": true, "nvim": true, "emacs": true,
	"tar": true, "gtar": true, "rsync": true, "zip": true, "less": true, "more": true, "man": true,
	// Interpreters.
	"python": true, "perl": true, "ruby": true, "node": true, "nodejs": true, "deno": true, "bun": true, "php": true,
	"lua": true, "luajit": true, "tclsh": true, "wish": true, "awk": true, "gawk": true, "mawk": true, "nawk": true,
	"R": true, "Rscript": true, "osascript": true, "pwsh": true, "powershell": true, "make": true, "gdb-multiarch": true,
	"java": true, "go": true, "cargo": true, "npm": true, "npx": true, "pnpm": true, "yarn": true, "pip": true,
	// Clients with a command or option that runs a program or a shell.
	"socat": true, "nc": true, "ncat": true, "netcat": true, "sqlite3": true, "psql": true, "mysql": true,
	"mariadb": true, "redis-cli": true, "mongo": true, "mongosh": true, "ftp": true, "sftp": true, "scp": true,
	"git": true, "find": true, "crontab": true, "nginx": true,
}

// RemoteReadOf is the ssh.read_commands entry an ssh call runs as a read, or
// "" for a call that is not one.
func (e *Engine) RemoteReadOf(tool string, args json.RawMessage) string {
	host, command, ok := remoteCommand(tool, args)
	if !ok {
		return ""
	}
	rr := readRemote(host, command)
	rr.stdin = carriesStdinSecret(args)
	return e.remoteReadRule(rr)
}
