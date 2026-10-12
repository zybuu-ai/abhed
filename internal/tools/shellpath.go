package tools

import (
	"path"
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
	switch n {
	case "PATH":
		f.pathSet, f.pathUnknown = true, true
	case "BASH_ENV", "ENV":
		f.startupHidden = "a shell start-up file set by a command (" + n + ")"
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

// startups are the start-up files the shells the line starts read from the
// HOME and ZDOTDIR folders it sets, as they start; why says when one is not
// known. Neither counts when the line starts no shell and runs no script.
func (f *shellFacts) startups() (files []Script, why string) {
	if !f.shellRun && len(f.scripts) == 0 {
		return nil, ""
	}
	for _, d := range f.startupDirs {
		for _, name := range startupDirFiles[d.name] {
			// What the shells started read; a script run may be zsh's.
			if f.homeFiles[name] || name == ".zshenv" && len(f.scripts) > 0 {
				files = append(files, Script{Path: path.Join(d.dir, name), Startup: true})
			}
		}
	}
	return files, f.startupDirHidden
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

// homeStartups are the start-up files, by name, the shells the line starts
// read from the HOME they inherit: none when the line sets HOME itself, and
// no zsh file when it sets ZDOTDIR, as those folders are read instead.
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
