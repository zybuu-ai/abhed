package tools

import (
	"maps"
	"path"
	"regexp"
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
		f.anyShell(true)
		for i, w := range words {
			if tmuxRuns[w] {
				f.commandWords(args[i+1:], w == "send-keys" || w == "send", depth)
				break
			}
		}
	case "screen":
		f.anyShell(true)
		f.commandWords(args, false, depth)
	case "script":
		if has(short('c')) || has(func(w string) bool { return strings.HasPrefix(w, "--command") }) {
			return
		}
		// Its shell, or the command it is given, runs on a terminal of its own.
		f.anyShell(false)
		for i, w := range words {
			if !strings.HasPrefix(w, "-") { // the log file; a command may follow
				f.commandWords(args[i+1:], false, depth)
				break
			}
		}
	case "unbuffer", "dtach":
		f.anyShell(false)
		f.commandWords(args, false, depth)
	case "expect":
		f.startupHidden = "expect, whose programs and their start-up files the check cannot read"
	case "doas":
		if has(short('s')) {
			f.anyShell(false)
		}
	case "ssh":
		if sshCommand(words) == -1 {
			f.startupHidden = "ssh with no command, whose login shell reads start-up files the check cannot read"
		}
	case "login":
		f.startupHidden = "login, whose shell reads another account's start-up files"
	case "su":
		switch {
		case has(func(w string) bool { return w == "-" || w == "--login" }) || has(short('l')):
			f.startupHidden = "su -, whose login shell reads another account's start-up files"
		case !has(short('c')) && !has(func(w string) bool { return strings.HasPrefix(w, "--command") }):
			f.anyShell(false)
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
			f.anyShell(false)
		}
	}
}

// anyShell records an interactive shell, login too when login is set, of
// any kind, reading every start-up file such shells read.
func (f *shellFacts) anyShell(login bool) {
	f.shellRun, f.interactive = true, true
	if f.homeFiles == nil {
		f.homeFiles = map[string]bool{}
	}
	for _, files := range homeStartFiles {
		for _, n := range append(append([]string(nil), files[0]...), files[2]...) {
			f.homeFiles[n] = true
		}
		if login {
			for _, n := range files[1] {
				f.homeFiles[n] = true
			}
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

// tmuxRuns are the tmux commands that run a command or type keys into a shell.
var tmuxRuns = map[string]bool{"new-session": true, "new": true, "new-window": true, "neww": true,
	"split-window": true, "splitw": true, "respawn-pane": true, "respawnp": true, "respawn-window": true,
	"respawnw": true, "run-shell": true, "run": true, "send-keys": true, "send": true, "display-popup": true,
	"popup": true, "if-shell": true, "if": true, "pipe-pane": true, "pipep": true}

// maxCommandStarts bounds where a command given to a terminal program is
// taken to start.
const maxCommandStarts = 8

// commandWords reads the command a terminal program runs, as bash -c text,
// from each word that may start it, so the rules see it whatever options
// come first; keys drops tmux's key names.
func (f *shellFacts) commandWords(args []*syntax.Word, keys bool, depth int) {
	var words []string
	for _, a := range args {
		v, lit := literal(a)
		if !lit {
			f.expands, f.ifsUnknown = true, true
			return
		}
		if keys && (tmuxKey.MatchString(v) || v == "Enter" || v == "KPEnter" || v == "Escape" || v == "Space") {
			continue
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

// tmuxKey is a key name such as C-m or M-x.
var tmuxKey = regexp.MustCompile(`^[CMS]-.$`)

// sshCommand is the index of the first word of the command ssh runs, or -1
// for none: ssh then starts a login shell.
func sshCommand(words []string) int {
	host := false
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "--":
		case strings.HasPrefix(w, "-") && len(w) == 2 && strings.IndexByte("BbcDEeFIiJLlmOoPpQRSWw", w[1]) >= 0:
			i++
		case strings.HasPrefix(w, "-"):
		case !host:
			host = true
		default:
			return i
		}
	}
	return -1
}
