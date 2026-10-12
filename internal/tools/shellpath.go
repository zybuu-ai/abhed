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

// startups are the start-up files a shell the line runs may read from the
// HOME and ZDOTDIR folders it sets; why says when one is not known. Neither
// counts when the line starts no shell and runs no script.
func (f *shellFacts) startups() (files []Script, why string) {
	if !f.shellRun && len(f.scripts) == 0 {
		return nil, ""
	}
	for _, d := range f.startupDirs {
		for _, name := range startupDirFiles[d.name] {
			files = append(files, Script{Path: path.Join(d.dir, name), Startup: true})
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
