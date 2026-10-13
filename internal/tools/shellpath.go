package tools

import (
	"maps"
	"path"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// maxBare bounds the programs named without a folder that a command keeps
// to look up on a PATH it sets.
const maxBare = 32

// Markers for the expansions a PATH value may hold, as pathValue reads it.
const (
	pathInherited = "\x01PATH\x01"
	pathHere      = "\x01PWD\x01"
	pathBuilt     = "\x00"
)

// setsVar reads an assignment that changes which file runs: PATH, the
// start-up file BASH_ENV or ENV names, or the folder HOME or ZDOTDIR names.
func (f *shellFacts) setsVar(a *syntax.Assign) {
	if a.Name == nil || a.Naked {
		return
	}
	switch a.Name.Value {
	case "PATH":
		f.pathSet = true
		if a.Value == nil && a.Array == nil {
			f.pathEntries("") // PATH= searches the current folder
			return
		}
		if a.Value == nil || a.Index != nil || a.Array != nil {
			f.pathUnknown = true
			return
		}
		f.pathEntries(pathValue(a.Value))
	case "BASH_ENV", "ENV":
		v, lit := "", false
		if a.Value != nil && a.Index == nil && a.Array == nil {
			v, lit = literal(a.Value)
		}
		f.startupFile(a.Name.Value, v, lit)
	case "HOME", "ZDOTDIR":
		v, lit := "", a.Value == nil && a.Index == nil && a.Array == nil
		if a.Value != nil && a.Index == nil && a.Array == nil {
			v, lit = literal(a.Value)
		}
		f.startupDir(a.Name.Value, v, lit)
	}
}

// setsByName reads a name a command sets by name (read, printf -v, for,
// declare -n): such a PATH has a value the text does not show.
func (f *shellFacts) setsByName(v string) {
	n, _, _ := strings.Cut(v, "=")
	n, _, _ = strings.Cut(strings.TrimSuffix(n, "+"), "[")
	f.forgetVar(n)
	f.setsEnv = true
	switch n {
	case "PATH":
		f.pathSet, f.pathUnknown = true, true
	case "BASH_ENV":
		f.startupHidden = "a shell start-up file set by a command (" + n + ")"
	case "ENV":
		f.envHidden = "a shell start-up file set by a command (ENV)"
	case "HOME", "ZDOTDIR":
		f.startupDir(n, "", false)
	}
}

// envAssign reads a NAME=value word env is given.
func (f *shellFacts) envAssign(w *syntax.Word) {
	v, lit := literal(w)
	if !lit {
		v = wordText(w)
	}
	name, value, _ := strings.Cut(v, "=")
	f.setsEnv = true
	if f.callEnv != nil {
		if lit {
			f.callEnv[name] = value
		} else {
			delete(f.callEnv, name)
		}
	}
	switch name {
	case "PATH":
		f.pathSet = true
		if !lit {
			f.pathUnknown = true
			return
		}
		f.pathEntries(value)
	case "BASH_ENV", "ENV":
		f.startupFile(name, value, lit)
	case "HOME", "ZDOTDIR":
		f.startupDir(name, value, lit)
	}
}

// startupFile records the file a shell started later reads first, read as a
// script it runs; one named by an expansion is a script no rule can read.
func (f *shellFacts) startupFile(name, value string, lit bool) {
	f.setsEnv, f.varsDead = true, true
	switch {
	case name == "ENV" && !lit:
		// Only an interactive shell reads ENV: kept for startups to weigh.
		f.envHidden = "a shell start-up file named by an expansion (ENV)"
	case name == "ENV":
		if value != "" && value != "/dev/null" {
			f.envFiles = append(f.envFiles, value)
		}
	case !lit:
		f.startupHidden = "a shell start-up file named by an expansion (" + name + ")"
	case value != "" && value != "/dev/null":
		f.script(value, false)
	}
}

// maxStartupDirs bounds the HOME and ZDOTDIR folders one command keeps.
const maxStartupDirs = 4

// startupDirFiles are the files zsh reads from $ZDOTDIR, and the files zsh,
// bash, sh and ksh read from $HOME, as they start, log in or run as a
// terminal's shell, and as a login shell ends.
var startupDirFiles = map[string][]string{
	"ZDOTDIR": {".zshenv", ".zprofile", ".zshrc", ".zlogin", ".zlogout"},
	"HOME": {".zshenv", ".zprofile", ".zshrc", ".zlogin", ".zlogout",
		".bash_profile", ".bash_login", ".profile", ".bashrc", ".bash_logout", ".kshrc", ".mkshrc"},
}

// startupDir records a folder HOME or ZDOTDIR names, whose start-up files a
// shell the line starts reads; an empty value is taken as the current folder.
func (f *shellFacts) startupDir(name, value string, lit bool) {
	switch {
	case !lit:
		f.startupDirHidden = "a shell start-up folder named by an expansion (" + name + ")"
	case len(f.startupDirs) >= maxStartupDirs:
		f.startupDirHidden = "more shell start-up folders than the check reads (" + name + ")"
	default:
		if value == "" {
			value = "."
		}
		f.startupDirs = append(f.startupDirs, startupDirSet{name, value})
	}
}

// startupDirSet is a folder named by HOME or ZDOTDIR.
type startupDirSet struct{ name, dir string }

// startups are the start-up files the line's shells read from the HOME and
// ZDOTDIR it sets, by how they start; why says when one is not known.
func (f *shellFacts) startups() (files []Script, why string) {
	if !f.shellRun && len(f.scripts) == 0 {
		return nil, ""
	}
	for _, d := range f.startupDirs {
		for _, name := range startupDirFiles[d.name] {
			// What the shells started read; a script run may be zsh's.
			if f.homeFiles[name] || name == ".zshenv" && len(f.scripts) > 0 {
				files = append(files, Script{Path: path.Join(d.dir, name), Startup: true, Env: f.startEnv()})
			}
		}
	}
	why = f.startupDirHidden
	if f.interactive {
		for _, e := range f.envFiles {
			files = append(files, Script{Path: e, Env: f.startEnv()})
		}
		if why == "" {
			why = f.envHidden
		}
	}
	return files, why
}

// pathEntries records the folders a PATH value names; an empty entry or
// $PWD is the current folder, and another expansion leaves them unknown.
func (f *shellFacts) pathEntries(value string) {
	for _, e := range strings.Split(value, ":") {
		switch {
		case e == pathInherited:
		case e == "" || e == pathHere:
			f.pathDirs = append(f.pathDirs, ".")
		case strings.HasPrefix(e, pathHere+"/") && !strings.ContainsAny(e[len(pathHere):], "\x00\x01"):
			f.pathDirs = append(f.pathDirs, path.Join(".", e[len(pathHere):]))
		case strings.ContainsAny(e, "\x00\x01") || strings.HasPrefix(e, "~"):
			f.pathUnknown = true
		default:
			f.pathDirs = append(f.pathDirs, e)
		}
	}
}

// pathValue is a PATH value's text with $PATH and $PWD marked, and any
// other expansion marked as built.
func pathValue(w *syntax.Word) string {
	var b strings.Builder
	var parts func([]syntax.WordPart)
	parts = func(ps []syntax.WordPart) {
		for _, p := range ps {
			switch p := p.(type) {
			case *syntax.Lit:
				b.WriteString(unescape(p.Value, ""))
			case *syntax.SglQuoted:
				if p.Dollar {
					b.WriteString(pathBuilt)
				} else {
					b.WriteString(p.Value)
				}
			case *syntax.DblQuoted:
				parts(p.Parts)
			case *syntax.ParamExp:
				switch {
				case simpleParam(p) && p.Param.Value == "PATH":
					b.WriteString(pathInherited)
				case simpleParam(p) && p.Param.Value == "PWD":
					b.WriteString(pathHere)
				default:
					b.WriteString(pathBuilt)
				}
			default:
				b.WriteString(pathBuilt)
			}
		}
	}
	parts(w.Parts)
	return b.String()
}

// bareProgram records a program named without a folder, which a PATH the
// command sets may find in a folder the agent wrote.
func (f *shellFacts) bareProgram(name string) {
	if len(f.bare) >= maxBare {
		f.bareCut = true
		return
	}
	f.bare = append(f.bare, name)
}

// homeStartFiles are what each shell reads from $HOME as it starts: always,
// as a login shell, and as an interactive one.
var homeStartFiles = map[string][3][]string{
	"zsh":  {{".zshenv"}, {".zprofile", ".zlogin", ".zlogout"}, {".zshrc"}},
	"bash": {nil, {".bash_profile", ".bash_login", ".profile", ".bash_logout"}, {".bashrc"}},
	"sh":   {nil, {".profile"}, nil},
	"dash": {nil, {".profile"}, nil},
	"ash":  {nil, {".profile"}, nil},
	"ksh":  {nil, {".profile"}, {".kshrc"}},
	"mksh": {nil, {".profile"}, {".mkshrc"}},
}

// shellStart records the start-up files a shell started with args reads
// from the HOME it inherits: by its options, -l, -i, --login, -o login.
func (f *shellFacts) shellStart(name string, args []*syntax.Word) {
	files, ok := homeStartFiles[name]
	if !ok {
		return
	}
	f.shellEnv = maps.Clone(f.callEnv)
	login, interactive, command, rc, profile, operand := false, false, false, true, true, false
scan:
	for j := 0; j < len(args); j++ {
		v, lit := literal(args[j])
		switch {
		case !lit && command:
			break scan // -c's text
		case !lit:
			// An option built when it runs may be either.
			login, interactive = true, true
			break scan
		case v == "--":
			operand = j+1 < len(args)
			break scan
		case v == "--login" || v == "-login":
			login = true
		case v == "--interactive":
			interactive = true
		case v == "--norc" && name == "bash":
			rc = false
		case v == "--noprofile" && name == "bash":
			profile = false
		case v == "--rcfile" || v == "--init-file":
			j, rc = j+1, false // read instead of .bashrc, as a file the line names
		case strings.HasPrefix(v, "--"):
		case len(v) > 1 && (v[0] == '-' || v[0] == '+'):
			for _, o := range v[1:] {
				switch o {
				case 'l':
					login = login || v[0] == '-'
				case 'i':
					interactive = interactive || v[0] == '-'
				case 'c':
					command = command || v[0] == '-'
				case 'o', 'O':
					if j++; j < len(args) {
						w, _ := literal(args[j])
						login = login || w == "login"
						interactive = interactive || w == "interactive"
					}
				}
			}
		default:
			operand = true
			break scan
		}
	}
	f.interactive = f.interactive || interactive
	f.ttyShell = f.ttyShell || !command && !operand
	// With no command and no script it reads its input: a terminal's, typed at one.
	if !command && !operand && rc {
		if f.ttyFiles == nil {
			f.ttyFiles = map[string]bool{}
		}
		for _, n := range files[2] {
			f.ttyFiles[n] = true
		}
	}
	if f.homeFiles == nil {
		f.homeFiles = map[string]bool{}
	}
	add := func(names []string) {
		for _, n := range names {
			f.homeFiles[n] = true
		}
	}
	add(files[0])
	if login && profile {
		add(files[1])
	}
	if interactive && rc {
		add(files[2])
	}
}

// homeStartups are the start-up files the line's shells read from the HOME
// they inherit; none for a HOME, and no zsh's for a ZDOTDIR, it sets.
func (f *shellFacts) homeStartups() []string {
	var out []string
	setsHome, setsZdot := f.startupDirHidden != "", f.startupDirHidden != ""
	for _, d := range f.startupDirs {
		setsHome = setsHome || d.name == "HOME"
		setsZdot = setsZdot || d.name == "ZDOTDIR"
	}
	if setsHome {
		return nil
	}
	for _, n := range startupDirFiles["HOME"] {
		if f.homeFiles[n] && (!setsZdot || !strings.HasPrefix(n, ".z")) {
			out = append(out, n)
		}
	}
	return out
}

// HomeStartupNames are the start-up files a shell may read from $HOME.
func HomeStartupNames() []string { return append([]string(nil), startupDirFiles["HOME"]...) }

// ptyShell reads a program that starts an interactive shell of the person's
// own choosing (script, tmux, screen, su, sudo -s) as starting every shell
// that reads start-up files; one in another account's home (su -, sudo -i,
// login) cannot be read, so it hides them.
func (f *shellFacts) ptyShell(name string, args []*syntax.Word, depth int) {
	switch name {
	case "tmux", "screen", "script", "login", "su", "sudo", "ssh", "doas", "expect", "unbuffer", "dtach", "byobu":
	default:
		return
	}
	words := make([]string, 0, len(args))
	for _, a := range args {
		v, lit := literal(a)
		if !lit {
			v = "\x00"
		}
		words = append(words, v)
	}
	has := func(match func(string) bool) bool { return slices.ContainsFunc(words, match) }
	short := func(o byte) func(string) bool {
		return func(w string) bool { return len(w) > 1 && w[0] == '-' && w[1] != '-' && strings.IndexByte(w, o) > 0 }
	}
	switch name {
	case "tmux", "byobu":
		switch full, why := tmuxRefusal(words); {
		case why != "":
			f.scriptHidden = why
		case tmuxEnds[full]:
			f.askUnderRules = "tmux " + full + " ends sessions, windows or panes, which may be your own"
		}
	case "screen":
		if why := screenRefusal(words); why != "" {
			f.scriptHidden = why
		}
	case "script":
		// Its shell, or the command after the log file, runs on a terminal of
		// its own; -c's text is run by a shell given -c.
		if !f.scriptCommand(args, depth) {
			f.anyShell()
		}
	case "unbuffer", "dtach":
		f.anyShell()
		f.commandWords(args, depth)
	case "expect":
		f.startupHidden = "expect, whose programs and their start-up files the check cannot read"
	case "doas":
		if has(short('s')) {
			f.anyShell()
		}
	case "ssh":
		if sshShell(words) {
			f.startupHidden = "ssh with no command, whose login shell reads start-up files the check cannot read"
		}
	case "login":
		f.startupHidden = "login, whose shell reads another account's start-up files"
	case "su":
		switch {
		case has(func(w string) bool { return w == "-" || w == "--login" }) || has(short('l')):
			f.startupHidden = "su -, whose login shell reads another account's start-up files"
		case !has(short('c')) && !has(func(w string) bool { return strings.HasPrefix(w, "--command") }):
			f.anyShell()
		}
	case "sudo":
		shell := false
		for i := 0; i < len(words); i++ {
			w := words[i]
			switch {
			case w == "-i" || w == "--login" || strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "i"):
				f.startupHidden = "sudo -i, whose login shell reads another account's start-up files"
				return
			case w == "-s" || w == "--shell" || strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "s"):
				shell = true
			case slices.Contains([]string{"-u", "-g", "-C", "-D", "-h", "-p", "-r", "-t", "-U", "-T"}, w):
				i++
			case strings.HasPrefix(w, "-"):
			default:
				return // a command: sudo -s runs it with -c
			}
		}
		if shell {
			f.anyShell()
		}
	}
}

// anyShell records an interactive shell of any kind, reading every start-up
// file such shells read.
func (f *shellFacts) anyShell() {
	f.shellRun, f.interactive = true, true
	if f.homeFiles == nil {
		f.homeFiles = map[string]bool{}
	}
	for _, files := range homeStartFiles {
		for _, n := range append(append([]string(nil), files[0]...), files[2]...) {
			f.homeFiles[n] = true
		}
	}
}

// startEnv is the environment the line's shells start with.
func (f *shellFacts) startEnv() map[string]string {
	if f.shellEnv != nil {
		return f.shellEnv
	}
	return f.commandEnv(nil)
}

// tmuxCommands are tmux's commands by full name, each with its alias; tmux
// takes any unique prefix of a full name too.
var tmuxCommands = map[string]string{"attach-session": "attach", "bind-key": "bind", "break-pane": "breakp",
	"capture-pane": "capturep", "choose-buffer": "", "choose-client": "", "choose-tree": "", "clear-history": "clearhist",
	"clear-prompt-history": "clearphist", "clock-mode": "", "command-prompt": "", "confirm-before": "confirm",
	"copy-mode": "", "customize-mode": "", "delete-buffer": "deleteb", "detach-client": "detach", "display-menu": "menu",
	"display-message": "display", "display-popup": "popup", "display-panes": "displayp", "find-window": "findw",
	"has-session": "has", "if-shell": "if", "join-pane": "joinp", "kill-pane": "killp", "kill-server": "",
	"kill-session": "", "kill-window": "killw", "last-pane": "lastp", "last-window": "last", "link-window": "linkw",
	"list-buffers": "lsb", "list-clients": "lsc", "list-commands": "lscm", "list-keys": "lsk", "list-panes": "lsp",
	"list-sessions": "ls", "list-windows": "lsw", "load-buffer": "loadb", "lock-client": "lockc", "lock-server": "lock",
	"lock-session": "locks", "move-pane": "movep", "move-window": "movew", "new-session": "new", "new-window": "neww",
	"next-layout": "nextl", "next-window": "next", "paste-buffer": "pasteb", "pipe-pane": "pipep",
	"previous-layout": "prevl", "previous-window": "prev", "refresh-client": "refresh", "rename-session": "rename",
	"rename-window": "renamew", "resize-pane": "resizep", "resize-window": "resizew", "respawn-pane": "respawnp",
	"respawn-window": "respawnw", "rotate-window": "rotatew", "run-shell": "run", "save-buffer": "saveb",
	"select-layout": "selectl", "select-pane": "selectp", "select-window": "selectw", "send-keys": "send",
	"send-prefix": "", "server-access": "", "set-buffer": "setb", "set-environment": "setenv", "set-hook": "",
	"set-option": "set", "set-window-option": "setw", "show-buffer": "showb", "show-environment": "showenv",
	"show-hooks": "", "show-messages": "showmsgs", "show-options": "show", "show-prompt-history": "showphist",
	"show-window-options": "showw", "source-file": "source", "split-window": "splitw", "start-server": "start",
	"suspend-client": "suspendc", "swap-pane": "swapp", "swap-window": "swapw", "switch-client": "switchc",
	"unbind-key": "unbind", "unlink-window": "unlinkw", "wait-for": "wait"}

// tmuxReadOnly are the tmux commands that only list or show what a running
// server holds and start none: under a rule for bash, the only ones allowed.
// attach-session, list-keys and list-commands start one, which reads its
// configuration; kill-* are asked about, as they end the person's sessions.
var tmuxReadOnly = map[string]bool{"list-sessions": true, "list-windows": true, "list-panes": true,
	"list-clients": true, "list-buffers": true, "has-session": true, "detach-client": true, "show-options": true,
	"show-window-options": true, "show-environment": true, "show-messages": true, "show-hooks": true,
	"show-buffer": true, "capture-pane": true}

// tmuxEnds are the tmux commands that end sessions, windows or panes.
var tmuxEnds = map[string]bool{"kill-session": true, "kill-server": true, "kill-window": true, "kill-pane": true}

// tmuxResolve is the full name tmux runs for name: an alias, a full name,
// or a unique prefix of one; "" when none or more than one match.
func tmuxResolve(name string) string {
	for full, alias := range tmuxCommands {
		if name == full || name == alias && alias != "" {
			return full
		}
	}
	match := ""
	for full := range tmuxCommands {
		if strings.HasPrefix(full, name) {
			if match != "" {
				return ""
			}
			match = full
		}
	}
	return match
}

// tmuxRefusal is the command a tmux line runs, and why it is not one of the
// allowed ones: it could start a shell, run a command, type keys or read a
// configuration.
func tmuxRefusal(words []string) (full, why string) {
	i := 0
	for ; i < len(words) && strings.HasPrefix(words[i], "-") && words[i] != "--"; i++ {
		w := words[i]
		for k := 1; k < len(w); k++ {
			switch {
			case w[k] == 'V':
				return "", "" // prints its version, runs nothing
			case strings.IndexByte("fLSc", w[k]) >= 0:
				return "", "tmux " + w + " names a configuration, socket or command the check cannot read"
			case w[k] == 'T':
				if k == len(w)-1 {
					i++
				}
				k = len(w)
			}
		}
	}
	if i < len(words) && words[i] == "--" {
		i++
	}
	if i >= len(words) {
		return "", "tmux with no command starts a session, whose shell and configuration the check cannot read"
	}
	for _, w := range words[i:] {
		if w == "\x00" || strings.HasSuffix(w, ";") {
			return "", "tmux is given a command built when it runs, or more than one"
		}
	}
	full = tmuxResolve(words[i])
	if tmuxEnds[full] {
		return full, ""
	}
	if !tmuxReadOnly[full] {
		return "", "tmux " + words[i] + " is not one of its read-only commands; what it runs, types or reads cannot be checked"
	}
	return full, ""
}

// screenRefusal says why a screen line is not a listing, a wipe or a
// reattach, the only forms allowed under a rule for bash.
func screenRefusal(words []string) string {
	mode, operands := "", 0
	for i := 0; i < len(words); i++ {
		switch w := words[i]; {
		case w == "-ls" || w == "-list" || w == "-wipe" || w == "-r" || w == "-x" || w == "-v" || w == "-version":
			mode = w
		case w == "-S" || w == "-p":
			i++
		case w == "\x00" || strings.HasPrefix(w, "-"):
			return "screen " + w + " runs, types or reads what the check cannot see"
		default:
			operands++
		}
	}
	if mode == "" || operands > 1 {
		return "screen starting a session or given a command runs what the check cannot see"
	}
	return ""
}

// maxCommandStarts bounds where a command given to a terminal program is
// taken to start.
const maxCommandStarts = 8

// commandWords reads the command a terminal program runs, as bash -c text,
// from each word that may start it, so the rules see it whatever options
// come first.
func (f *shellFacts) commandWords(args []*syntax.Word, depth int) {
	var words []string
	for _, a := range args {
		v, lit := literal(a)
		if !lit {
			f.expands, f.ifsUnknown = true, true
			return
		}
		words = append(words, v)
	}
	starts := 0
	for i, w := range words {
		if strings.HasPrefix(w, "-") {
			continue
		}
		if starts++; starts > maxCommandStarts {
			return
		}
		f.read(strings.Join(words[i:], " "), depth+1)
	}
}

// sshValued are ssh's option letters that take a value.
const sshValued = "BbcDEeFIiJLlmOoPpQRSWw"

// sshShell reports an ssh that starts a login shell: one with no command and
// none of the options that start none (-V, -G, -Q, -O, -N, -T).
func sshShell(words []string) bool {
	host := false
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "--":
		case len(w) > 1 && w[0] == '-':
			for k := 1; k < len(w); k++ {
				if strings.IndexByte("VGQONT", w[k]) >= 0 {
					return false
				}
				if strings.IndexByte(sshValued, w[k]) >= 0 {
					if k == len(w)-1 {
						i++ // its value is the next word
					}
					break
				}
			}
		case !host:
			host = true
		default:
			return false // a command
		}
	}
	return host
}

// scriptValued are script's options that take the next word as a value.
const scriptValued = "FtTBIOo"

// scriptCommand reads the command script runs, as if typed: -c's text, or
// the words after the log file, each quoted as given. It reports -c.
func (f *shellFacts) scriptCommand(args []*syntax.Word, depth int) bool {
	for i := 0; i < len(args); i++ {
		v, lit := literal(args[i])
		switch {
		case !lit:
			f.expands, f.ifsUnknown = true, true
			return false
		case v == "--command" || len(v) > 1 && v[0] == '-' && v[1] != '-' && strings.HasSuffix(v, "c"):
			if i+1 < len(args) {
				f.shellText(args[i+1:i+2], depth)
			}
			return true
		case strings.HasPrefix(v, "--command="):
			f.read(strings.TrimPrefix(v, "--command="), depth+1)
			return true
		case len(v) > 1 && v[0] == '-' && v[1] != '-' && strings.ContainsAny(v[len(v)-1:], scriptValued):
			i++
		case strings.HasPrefix(v, "-"):
		default:
			// The log file; what follows is the command, argument by argument.
			var quoted []string
			for _, a := range args[i+1:] {
				w, lit := literal(a)
				if !lit {
					f.expands, f.ifsUnknown = true, true
					return false
				}
				quoted = append(quoted, "'"+strings.ReplaceAll(w, "'", `'\''`)+"'")
			}
			if len(quoted) > 0 {
				f.read(strings.Join(quoted, " "), depth+1)
			}
			return false
		}
	}
	return false
}
