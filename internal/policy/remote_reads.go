package policy

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
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
	control := hasShellControl(s)
	if control && knownReads[rawProgram(s)].quotedValues && !hasShellControl(withoutSingleQuoted(s)) && !backslashInSingleQuotes(s) {
		control = false // a query format's ${…}, in single quotes
	}
	if control || canon.Reworded || canon.Unparsed() || canon.Incomplete() {
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
		return Rule{}, fmt.Errorf("ssh.read_commands: %q is not a read form of %s. %s; %s "+
			"(see docs/ops/infrastructure.md#ssh); or declare the exact command instead", p, name, acceptedForms, readFormSummary(name, spec))
	case !wildcard && known && !readCallOK(spec, words[1:], func(string) bool { return false }):
		return Rule{}, fmt.Errorf("ssh.read_commands: %q is not a read form of %s: it uses a verb or option the list does not hold as a read; %s "+
			"(see docs/ops/infrastructure.md#ssh)", p, name, readFormSummary(name, spec))
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

// acceptedForms says what a wildcard entry may be, for an entry refused at load.
const acceptedForms = "An entry is an exact command (\"df -h\"), or a pattern whose * or ? stands for " +
	"a whole operand (\"df -h *\", \"tail -n 50 /var/log/*.log\") or for an option's value after = " +
	"(\"df --output=*\"), never for part of an option (\"-h*\") or for an option itself; " +
	"either stands within one argument, and a word that is ** alone stands for one or more whole arguments (\"systemctl status **\")"

// remoteReadRule is the ssh.read_commands entry the call's command matches,
// or "": only one simple command, whose words the text shows, that no
// check reads as destructive.
func (e *Engine) remoteReadRule(rr remoteRead) string {
	if len(e.RemoteReads) == 0 || !rr.complete || rr.canon.Hidden || rr.canon.IFSSplit {
		return ""
	}
	// A query format's ${…} in single quotes is read only for the forms that take one.
	quotedFormat := rr.quotedOnly && knownReads[rawProgram(rr.command)].quotedValues
	if !rr.narrow && !quotedFormat {
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
		if !strings.ContainsAny(r.glob, "*?") {
			return r.raw
		}
		if plain && wildcardReadOK(text) && wordsMatch(r.glob, text) && (e.ReadCommandsContents || !DoubleStarReadsContents(r.glob, text)) {
			return r.raw
		}
	}
	return ""
}

// DoubleStarReadsContents reports an entry with a word that is ** alone
// whose call, text, prints the contents of files; text "" asks of the
// entry itself. Such an entry could name any file the account can read,
// the reach of ssh.read_contents, so it matches such a call only where
// that is set.
func DoubleStarReadsContents(entry, text string) bool {
	words := callWords(entry)
	double := false
	for _, w := range words {
		double = double || w.raw == "**"
	}
	if !double {
		return false
	}
	if text != "" {
		words = callWords(text)
	}
	args := make([]string, 0, len(words))
	for _, w := range words[1:] {
		if w.raw != "**" {
			args = append(args, w.val)
		}
	}
	return printsContents(rawProgram(entry), args)
}

// callWord is one word of a command: as written, as the program gets it
// once quotes are taken off, and whether the shell could expand it (an
// unquoted *, ?, [, brace or ~, or a $ outside single quotes).
type callWord struct {
	raw, val string
	expands  bool
}

// callWords splits s as a POSIX shell does, quotes and backslashes taken
// off, saying of each word whether the shell could expand it.
func callWords(s string) []callWord {
	var out []callWord
	var raw, val strings.Builder
	in, expands := false, false
	var quote byte
	flush := func() {
		if in {
			out = append(out, callWord{raw: raw.String(), val: val.String(), expands: expands})
		}
		raw.Reset()
		val.Reset()
		in, expands = false, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			raw.WriteByte(c)
			if c == '\'' {
				quote = 0
			} else {
				val.WriteByte(c)
			}
		case quote == '"':
			raw.WriteByte(c)
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(s):
				i++
				raw.WriteByte(s[i])
				val.WriteByte(s[i])
			default:
				expands = expands || c == '$' || c == '`'
				val.WriteByte(c)
			}
		case c == ' ' || c == '\t':
			flush()
		case c == '\'' || c == '"':
			raw.WriteByte(c)
			quote, in = c, true
		case c == '\\' && i+1 < len(s):
			raw.WriteByte(c)
			i++
			raw.WriteByte(s[i])
			val.WriteByte(s[i])
			in = true
		default:
			if strings.IndexByte("*?[{}~$`", c) >= 0 {
				expands = true
			}
			raw.WriteByte(c)
			val.WriteByte(c)
			in = true
		}
	}
	flush()
	return out
}

// wordsMatch reports a call whose words match a wildcard entry's in order.
//
// A * or ? stands within one argument, so "tail -n * /var/log/app/app.log"
// cannot also name /etc/shadow, and a word that is ** alone stands for one
// or more whole arguments, each still judged as a read form. A word a
// wildcard matches is taken as the program gets it, so it may hold nothing
// the remote shell would expand (a glob, a brace, ~ or $), unless it is
// the entry's own word as written; and none may hold a .. segment the
// entry does not write. Where a * stands for an operand, not an option's
// value, it never matches an option (a word starting with -) before a --:
// getopt would read one there as an option.
func wordsMatch(glob, text string) bool {
	want, got := callWords(glob), callWords(text)
	if len(want) == 0 || len(got) == 0 {
		return false
	}
	valueSlot := entryValueSlots(want)
	ended := make([]bool, len(got)+1) // ended[j]: a -- comes before got[j]
	for j, w := range got {
		ended[j+1] = ended[j] || w.val == "--"
	}
	ok := func(w callWord) bool { return !w.expands && !dotDot(w.val) }
	var match func(i, j int) bool
	match = func(i, j int) bool {
		if i == len(want) {
			return j == len(got)
		}
		if want[i].val == "**" && want[i].raw == "**" {
			for k := j + 1; k <= len(got); k++ {
				if !ok(got[k-1]) {
					return false
				}
				if match(i+1, k) {
					return true
				}
			}
			return false
		}
		if j >= len(got) {
			return false
		}
		w, g := want[i], got[j]
		if !strings.ContainsAny(w.val, "*?") {
			return w.val == g.val && (!g.expands || g.raw == w.raw) && match(i+1, j+1)
		}
		if g.raw != w.raw {
			if !ok(g) || !wordMatches(w.val, g.val) {
				return false
			}
			if !valueSlot[i] && !ended[j] && strings.HasPrefix(g.val, "-") {
				return false
			}
		}
		return match(i+1, j+1)
	}
	return match(0, 0)
}

// entryValueSlots marks the words of an entry that are an option's value:
// the word after an option of its program's read form that takes one.
func entryValueSlots(words []callWord) map[int]bool {
	out := map[int]bool{}
	spec, known := knownReads[tools.CommandName(path.Base(words[0].val))]
	if !known {
		return out
	}
	op, from := spec.opts, 1
	if spec.verbs != nil && len(words) > 1 {
		if vo, ok := spec.verbs[words[1].val]; ok {
			op, from = vo, 2
		}
	}
	for i := from; i < len(words)-1; i++ {
		w := words[i].val
		if w == "--" {
			break
		}
		if spec.oneDash && strings.HasPrefix(w, "--") {
			w = w[1:]
		}
		if strings.HasPrefix(w, "-") && op.values[w] {
			out[i+1] = true
		}
	}
	return out
}

// wordMatches reports one argument matching one word of an entry.
func wordMatches(w, arg string) bool {
	if dotDot(arg) && !dotDot(w) {
		return false
	}
	re, err := wordGlob(w)
	return err == nil && re.MatchString(arg)
}

// dotDot reports a word with a .. path segment.
func dotDot(w string) bool {
	for _, seg := range strings.Split(w, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// wordGlob is one word of an entry as a pattern for one argument: * is any
// text and ? one character, within that argument.
func wordGlob(w string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("(?s)^")
	for _, c := range w {
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// wildcardReadOK reports a call a wildcard entry may approve: its program
// on the read-only list, called in a form the list allows.
func wildcardReadOK(text string) bool {
	spec, known := knownReads[rawProgram(text)]
	if !known {
		return false
	}
	words, dollar, glob := shellWords(text)
	// The words are read as POSIX shells read them; a backslash inside single
	// quotes is read otherwise by fish, so such a call is not a read.
	if len(words) == 0 || dollar || glob || backslashInSingleQuotes(text) {
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
	// The remote is taken as Linux, where case matters, so a runner is found
	// in any case, whatever this host folds.
	name = strings.ToLower(name)
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
	"r": true, "rscript": true, "osascript": true, "pwsh": true, "powershell": true, "make": true, "gdb-multiarch": true,
	"java": true, "go": true, "cargo": true, "npm": true, "npx": true, "pnpm": true, "yarn": true, "pip": true,
	// Clients with a command or option that runs a program or a shell.
	"socat": true, "nc": true, "ncat": true, "netcat": true, "sqlite3": true, "psql": true, "mysql": true,
	"mariadb": true, "redis-cli": true, "mongo": true, "mongosh": true, "ftp": true, "sftp": true, "scp": true,
	"git": true, "find": true, "crontab": true, "nginx": true,
	// Others that run a program or code they are given.
	"setpriv": true, "prlimit": true, "capsh": true, "setarch": true, "linux32": true, "linux64": true,
	"run-parts": true, "start-stop-daemon": true, "sshpass": true, "m4": true, "dc": true, "bc": true,
	"julia": true, "ansible": true, "ansible-playbook": true, "ansible-console": true, "ansible-pull": true,
	"runcon": true, "bwrap": true, "fakeroot": true, "fakechroot": true, "chpst": true, "proot": true,
	"unbuffer": true, "daemonize": true, "daemon": true, "nsjail": true, "minijail0": true, "sandbox": true,
	"systemd-cat": true, "systemd-inhibit": true, "unshare-net": true, "faketime": true, "torsocks": true,
	"proxychains": true, "proxychains4": true, "ionice-wrapper": true, "chronic": true, "ifne": true,
	"vipe": true, "lockf": true, "su-exec": true, "gosu": true, "s6-setuidgid": true, "dropbearmulti": true,
	"emacsclient": true, "groovy": true, "scala": true, "kotlin": true, "elixir": true, "erl": true,
	"guile": true, "racket": true, "sbcl": true, "ocaml": true, "ghci": true, "runghc": true, "swift": true,
	"dotnet": true, "mono": true, "qjs": true, "jjs": true, "jshell": true, "nix-shell": true, "nix": true,
	"flatpak": true, "snap": true, "toolbox": true, "distrobox": true, "lxc": true, "lxc-exec": true,
	"tini": true, "dumb-init": true, "catatonit": true, "s6-svscan": true, "virsh": true, "incus": true, "ctr": true, "nerdctl": true, "runc": true, "crun": true, "oc": true, "helm": true,
}

// RemoteReadOf is the ssh.read_commands entry an ssh call runs as a read,
// ReadOnlyRule when the read-only classifier finds it one on a host the
// operator declared, or "" for a call that is not a read.
func (e *Engine) RemoteReadOf(tool string, args json.RawMessage) string {
	host, command, ok := remoteCommand(tool, args)
	if !ok {
		return ""
	}
	rr := readRemote(host, command)
	rr.stdin = carriesStdinSecret(args)
	if rule := e.remoteReadRule(rr); rule != "" {
		return rule
	}
	if e.remoteIsReadOnly(rr) {
		return ReadOnlyRule
	}
	return ""
}

// remoteIsReadOnly reports an ssh call the read-only classifier approves.
func (e *Engine) remoteIsReadOnly(rr remoteRead) bool {
	_, why := e.remoteReadOnly(rr)
	return why == ""
}
