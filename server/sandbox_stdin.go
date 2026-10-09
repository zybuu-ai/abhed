package server

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A command fed stdin may be a shell or an interpreter reading its commands
// from that stdin, which no rule ever sees. Where a deny or ask rule or a
// hook is in force such a command is refused as uncheckable; :attach judges
// what a shell is given line by line instead. The rule, for every simple
// command anywhere in the line (pipelines, lists, substitutions, functions):
//   - a word naming a program in stdinInterpreters, by its base name in any
//     case with a version suffix ignored (python3.12, perl5), refuses it
//     wherever it stands, as kubectl exec -i pod -- sh would run it; a name
//     also common as an argument (".", "r", "ed") only as the program or
//     after a wrapper;
//   - a program named by an expansion, a glob or $'…' refuses it, and so
//     does such a word after a program in stdinWrappers, which run another;
//   - a line that does not parse refuses it.
// A false match only refuses a command that could run without stdin.

// stdinInterpreters run commands they read, or build commands from it.
var stdinInterpreters = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`sh bash zsh dash ksh mksh pdksh ash yash csh tcsh fish rc es elvish nu xonsh posh
		busybox toybox eval source . python pypy ipython perl ruby irb jruby node nodejs deno bun php lua luajit
		tclsh wish expect r rscript julia osascript pwsh powershell guile racket sbcl clisp ecl gforth octave
		awk gawk mawk nawk sed gsed xargs parallel ed ex vi vim nvim view emacs gdb lldb sqlite psql mysql dc m4
		make gmake jshell groovy scala erl escript elixir iex ghci runghc ocaml swift ssh script`) {
		stdinInterpreters[n] = true
	}
}

// stdinProgramOnly are interpreters whose names are common arguments too.
var stdinProgramOnly = map[string]bool{".": true, "r": true, "ed": true, "ex": true, "es": true, "rc": true, "nu": true,
	"dc": true, "m4": true, "make": true, "script": true, "view": true, "source": true, "eval": true}

// stdinWrappers run the program named after them, so an expansion there
// could name an interpreter.
var stdinWrappers = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`env command exec builtin nohup nice timeout setsid stdbuf ionice taskset chrt
		prlimit sudo doas su runuser chroot unshare nsenter flock time strace ltrace valgrind watch coproc
		systemd-run firejail bwrap proot fakeroot numactl cgexec sg newgrp find`) {
		stdinWrappers[n] = true
	}
}

// versioned is a program name's version suffix.
var versioned = regexp.MustCompile(`[0-9][0-9.\-_]*$`)

// programName is a word as a program name: its base, lower case, unversioned.
func programName(word string) string {
	n := strings.ToLower(path.Base(strings.ReplaceAll(word, `\`, "")))
	if n == "." {
		return n
	}
	return versioned.ReplaceAllString(n, "")
}

// literalWord is a word's text when it has no expansion, glob or $'…'.
func literalWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, p := range w.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(p.Value, "*?[{") {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				l, ok := q.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(l.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// stdinReader names what in line could read commands from its stdin, "" when
// nothing does.
func stdinReader(line string) string {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(line), "")
	if err != nil {
		return "a line that cannot be read"
	}
	why := ""
	syntax.Walk(f, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok || why != "" || len(call.Args) == 0 {
			return why == ""
		}
		wrapped := false
		for i, w := range call.Args {
			text, lit := literalWord(w)
			switch {
			case !lit && (i == 0 || wrapped):
				why = "a program named by an expansion"
			case lit && stdinInterpreters[programName(text)] && (i == 0 || wrapped || !stdinProgramOnly[programName(text)]):
				why = programName(text)
			case i == 0 && stdinWrappers[programName(text)]:
				wrapped = true
			}
			if why != "" {
				break
			}
		}
		return why == ""
	})
	return why
}

// screensStdin reports whether a rule or hook in force could refuse some
// commands, so a command fed stdin must be checkable.
func screensStdin(pol *policy.Engine) bool {
	if pol.Screens("bash") {
		return true
	}
	for _, r := range pol.AskRules() {
		if r.ForTool("bash") {
			return true
		}
	}
	return false
}

// screenStdinExec judges a command fed stdin: the deny rules and hooks, then
// whether it could run commands from its stdin that they never see.
func (s *Server) screenStdinExec(pol *policy.Engine, command string) error {
	if err := denied(pol, command, false); err != nil {
		return err
	}
	if !screensStdin(pol) {
		return nil
	}
	what := stdinReader(command)
	if what == "" {
		return nil
	}
	instead := "open :attach (or an SSH shell), which judges each line, or pass the commands in the command line"
	if s.terminalRefusal(pol) != "" {
		instead = "pass each command in its own command line"
	}
	return fmt.Errorf("%w: %s reading commands from stdin cannot be checked against the rules and hooks in force; %s",
		controlplane.ErrForbidden, what, instead)
}
