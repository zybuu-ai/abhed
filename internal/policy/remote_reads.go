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
	// After a runner, a shell or an interpreter a wildcard is any command at all.
	if name := programName(s); strings.ContainsAny(s, "*?") && runsCommands(name) {
		return Rule{}, fmt.Errorf("ssh.read_commands: %q puts a wildcard after %s, which runs any command; name the command exactly", p, name)
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
	// Behind a runner, or with a find action that runs one, only an entry
	// written exactly matches: a wildcard there could reach any command.
	forms, whole := strippedForms(text)
	plain := whole && len(forms) == 1 && forms[0] == strings.Join(strings.Fields(text), " ") && !runsCommands(programName(text))
	for _, r := range e.RemoteReads {
		if (plain || !strings.ContainsAny(r.glob, "*?")) && r.pattern.MatchString(text) {
			return r.raw
		}
	}
	return ""
}

// RemoteReadWarning says why an ssh.read_commands entry may let a change run
// unasked: its wildcard follows a program that also writes or changes state
// (systemctl stop, journalctl --vacuum-size, openssl -out). "" when none.
func RemoteReadWarning(entry string) string {
	s := strings.TrimSpace(entry)
	if !strings.ContainsAny(s, "*?") {
		return ""
	}
	fields := strings.Fields(s)
	// systemctl with a reading verb written out stays a read, whatever follows.
	if name := programName(s); name == "systemctl" && len(fields) > 1 && systemctlReads[fields[1]] {
		return ""
	}
	if name := programName(s); changers[name] {
		return "the wildcard after " + name + " also matches the forms that change things on the host " +
			"(see the list not to declare in docs/ops/infrastructure.md); name the command exactly"
	}
	return ""
}

// systemctlReads are systemctl verbs that change nothing.
var systemctlReads = map[string]bool{"status": true, "show": true, "cat": true, "list-units": true,
	"list-unit-files": true, "list-timers": true, "is-active": true, "is-enabled": true, "is-failed": true}

// changers are programs a read-looking pattern can turn into a change.
var changers = map[string]bool{
	"systemctl": true, "service": true, "journalctl": true, "find": true, "openssl": true, "git": true,
	"tee": true, "dd": true, "cp": true, "mv": true, "ln": true, "install": true, "truncate": true, "touch": true,
	"mkdir": true, "chmod": true, "chown": true, "chgrp": true, "crontab": true, "ip": true, "iptables": true,
	"nft": true, "mount": true, "umount": true, "hostnamectl": true, "timedatectl": true, "loginctl": true,
	"apt": true, "apt-get": true, "dnf": true, "yum": true, "rpm": true, "dpkg": true, "pip": true, "npm": true,
	"curl": true, "wget": true, "sort": true, "uniq": true, "gzip": true, "gunzip": true, "unzip": true, "split": true,
	"logrotate": true, "sysctl": true, "kill": true, "pkill": true, "killall": true, "nginx": true, "certbot": true,
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

// programName is the program a command names first, by its base name, with
// a version suffix such as python3.12's dropped.
func programName(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	name := tools.CommandName(path.Base(fields[0]))
	if trimmed := strings.TrimRight(name, "0123456789."); trimmed != "" {
		return trimmed
	}
	return name
}

// runsCommands reports a program that runs a command or code it is given:
// a shell, an interpreter, or a runner such as sudo, su or systemd-run.
func runsCommands(name string) bool {
	_, wrapper := wrapperWords[name]
	return wrapper || commandRunners[name]
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
