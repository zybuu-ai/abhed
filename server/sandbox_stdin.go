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
// whether it could run commands from its stdin that they never see. A shell
// reading its commands from stdin is let through with in turned on, so each
// statement it is given is judged first; any other reader is refused.
func (s *Server) screenStdinExec(pol *policy.Engine, command string, env []string, in *stdinScreen) error {
	if err := denied(pol, command, false); err != nil {
		return err
	}
	if !screensStdin(pol) {
		return nil
	}
	what := stdinReader(command)
	if what == "" {
		return execEnvRefused(env)
	}
	if in != nil && stdinShell(command) && shellEnvVar(env, true) == "" {
		in.on = true
		return nil
	}
	instead := "open :attach (or an SSH shell), which judges each line, feed the commands to sh, whose input is judged statement by statement, or pass the commands in the command line"
	if s.terminalRefusal(pol) != "" {
		instead = "feed the commands to sh, whose input is judged statement by statement, or pass each command in its own command line"
	}
	return fmt.Errorf("%w: %s reading commands from stdin cannot be checked against the rules and hooks in force; %s",
		controlplane.ErrForbidden, what, instead)
}

// screenExec judges a command run without stdin: the deny rules and hooks,
// then the variables the exec sets itself.
func screenExec(pol *policy.Engine, command string, env []string) error {
	if err := denied(pol, command, false); err != nil {
		return err
	}
	if !screensStdin(pol) {
		return nil
	}
	return execEnvRefused(env)
}

// execEnvRefused refuses an exec's own variable that has any shell it starts
// run commands no rule sees, as BASH_ENV does even for a judged bash -c.
func execEnvRefused(env []string) error {
	if name := shellEnvVar(env, false); name != "" {
		return fmt.Errorf("%w: %s in the exec's environment has a shell run or read commands no rule or hook sees; leave it out",
			controlplane.ErrForbidden, name)
	}
	return nil
}

// shellEnvVar names the first of env a shell would act on unjudged: one in
// shellStartFiles, an exported function, or a name no variable has. A stdin
// shell also takes IFS, which changes how its statements split from how they
// were judged.
func shellEnvVar(env []string, stdin bool) string {
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case !envName.MatchString(name):
			return "a variable whose name is not an identifier"
		case shellStartFiles[name] || strings.HasPrefix(name, "BASH_FUNC_") || stdin && name == "IFS":
			return name
		}
	}
	return ""
}

// stdinShells are the shells whose input is judged statement by statement.
// zsh is not one: =cmd and glob qualifiers run what a bash parser reads as
// plain words, so its statements cannot be judged as it reads them.
var stdinShells = map[string]bool{"sh": true, "bash": true, "dash": true, "ksh": true, "mksh": true, "ash": true,
	"yash": true, "posh": true}

// stdinShellWrappers run the shell after them with the same input.
var stdinShellWrappers = map[string]bool{"env": true, "nice": true, "timeout": true, "exec": true, "nohup": true,
	"stdbuf": true, "setsid": true, "command": true}

// wrapperProgram is the index of the program a wrapper at words[i-1] runs,
// past the options and assignments it is known to take harmlessly, or -1 for
// anything else: env -S, which builds a new command line, an option of
// unknown arity, or a variable a shell runs on its own.
func wrapperProgram(wrapper string, words []string, i int) int {
	value := func(w, short, long string) (int, bool) { // how many words an option with a value takes
		switch {
		case w == short || w == long:
			return 2, true
		case strings.HasPrefix(w, short) && len(w) > len(short) || strings.HasPrefix(w, long+"="):
			return 1, true
		}
		return 0, false
	}
	for ; i < len(words); i++ {
		w := words[i]
		if w == "--" || w == "-" && wrapper == "env" { // env's - is -i and ends its options
			i++
			break
		}
		if !strings.HasPrefix(w, "-") || w == "-" {
			break
		}
		n := 1
		switch wrapper {
		case "env":
			if w == "-i" || w == "--ignore-environment" {
				break
			}
			var ok bool
			if n, ok = value(w, "-u", "--unset"); !ok {
				return -1
			}
		case "nice":
			if d, ok := value(w, "-n", "--adjustment"); ok {
				n = d
			} else if !niceWord.MatchString(w) {
				return -1
			}
		case "timeout":
			if w == "--preserve-status" || w == "--foreground" || w == "-v" || w == "--verbose" {
				break
			}
			d, ok := value(w, "-s", "--signal")
			if !ok {
				if d, ok = value(w, "-k", "--kill-after"); !ok {
					return -1
				}
			}
			n = d
		case "stdbuf":
			d, ok := value(w, "-i", "--input")
			if !ok {
				if d, ok = value(w, "-o", "--output"); !ok {
					if d, ok = value(w, "-e", "--error"); !ok {
						return -1
					}
				}
			}
			n = d
		case "exec": // -a and -l would make a login shell, which reads profile files
			if w != "-c" {
				return -1
			}
		case "setsid":
			if w != "-w" && w != "--wait" {
				return -1
			}
		case "command":
			if w != "-p" {
				return -1
			}
		default:
			return -1
		}
		i += n - 1
	}
	if wrapper == "env" {
		for ; i < len(words); i++ {
			if !strings.Contains(words[i], "=") {
				break
			}
			if shellEnvVar(words[i:i+1], true) != "" {
				return -1
			}
		}
	}
	if wrapper == "timeout" {
		if i >= len(words) || !durationWord.MatchString(words[i]) {
			return -1
		}
		i++
	}
	return i
}

// niceWord is nice's -N adjustment; envName a variable name.
var (
	niceWord = regexp.MustCompile(`^-[0-9]+$`)
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// shellStartFiles are variables naming files or text a shell runs on its
// own, unseen by the statement screen.
var shellStartFiles = map[string]bool{"BASH_ENV": true, "ENV": true, "ZDOTDIR": true, "SHELLOPTS": true, "BASHOPTS": true,
	"PS0": true, "PS1": true, "PS2": true, "PS3": true, "PS4": true, "PROMPT_COMMAND": true, "BASH_LOADABLES_PATH": true,
	"BASH_XTRACEFD": true, "EXECIGNORE": true}

// stdinShell reports whether command is only a shell reading its commands
// from stdin, such as sh, bash -s -- a b, env A=1 nice sh or busybox ash:
// one simple command of literal words, run directly or through wrappers,
// with no script file, no -c and no redirect of its input.
func stdinShell(command string) bool {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil || len(f.Stmts) != 1 {
		return false
	}
	st := f.Stmts[0]
	call, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || st.Background || st.Coprocess || len(call.Args) == 0 {
		return false
	}
	for _, r := range st.Redirs {
		if r.N == nil || r.N.Value == "0" {
			switch r.Op {
			case syntax.RdrIn, syntax.RdrInOut, syntax.DplIn, syntax.WordHdoc, syntax.Hdoc, syntax.DashHdoc:
				return false
			}
		}
	}
	for _, a := range call.Assigns {
		if shellEnvVar([]string{a.Name.Value + "="}, true) != "" {
			return false
		}
	}
	words := make([]string, len(call.Args))
	for i, w := range call.Args {
		text, lit := literalWord(w)
		if !lit {
			return false
		}
		words[i] = text
	}
	i := 0
	for i < len(words) && stdinShellWrappers[programName(words[i])] {
		if i = wrapperProgram(programName(words[i]), words, i+1); i < 0 {
			return false
		}
	}
	if i >= len(words) {
		return false
	}
	name := programName(words[i])
	if name == "busybox" && i+1 < len(words) {
		i++
		if name = programName(words[i]); name != "sh" && name != "ash" && name != "hush" {
			return false
		}
	} else if !stdinShells[name] {
		return false
	}
	return shellReadsStdin(words[i+1:])
}

// durationWord is a number, or a duration as timeout takes it.
var durationWord = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[smhd]?$`)

// shellReadsStdin reports whether a shell given args reads its commands
// from stdin. Only options known to leave that so are accepted, since a shell
// keeps parsing options after -s; anything else, a -c above all, is false.
func shellReadsStdin(args []string) bool {
	positional := false // -s: words after the options are its positional parameters
	for j := 0; j < len(args); j++ {
		a := args[j]
		switch {
		case a == "-" || a == "--":
			return positional || j+1 == len(args)
		case len(a) > 1 && a[0] == '-' && a[1] != '-' || len(a) > 1 && a[0] == '+':
			for _, o := range a[1:] {
				switch {
				case o == 's' && a[0] == '-':
					positional = true
				case o == 'o':
					if j++; j >= len(args) || !stdinShellSetOptions[args[j]] {
						return false
					}
				case o == 'e' || o == 'u' || o == 'x' || o == 'v':
				default:
					return false
				}
			}
		case strings.HasPrefix(a, "-") || a == "+":
			return false // a long option, which may name a file to run
		default:
			return positional // else a script file
		}
	}
	return true
}

// stdinShellSetOptions are the -o names a stdin shell may be given.
var stdinShellSetOptions = map[string]bool{"errexit": true, "nounset": true, "xtrace": true, "verbose": true, "pipefail": true}
