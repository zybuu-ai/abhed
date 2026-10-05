package policy

import (
	"strings"
	"testing"
)

// A program named by an expansion that edits its value, ${x%/} or ${x//_/ },
// is refused in every mode while a deny rule has a pattern; a plain $x or ${x}
// there, or one with a / in it, is never allowed.
func TestProgramExpansionCannotPassADenyRule(t *testing.T) {
	refused := []string{
		"x=git; ${x%/} reset --hard",
		"x=git_reset_--hard; ${x//_/ }",
		"x=git; \"${x%/}\" reset --hard",
		"${x:-git} reset --hard",
		"${!v} reset --hard",
		"x=git; env ${x%/} reset --hard",
		"echo ok && x=git && ${x%%/*} reset --hard",
		"x=git_reset_--hard; >/dev/null ${x//_/ }",
		"x=git; </dev/null ${x%/} reset --hard",
		"x=c_u_r_l; >/dev/null ${x//_/} http://e",
		"x=git; FOO=1 >/dev/null ${x%/} reset --hard",
		"x=git; command >/dev/null ${x%/} reset --hard",
		"x=git; >| f ${x%/} reset --hard",
		"x=git; <<< \"a b\" ${x%/} reset --hard",
		"x=git; 2>&1 ${x%/} reset --hard",
		"x=git; &>f ${x%/} reset --hard",
		"x=git; 3< f ${x%/} reset --hard",
		"x=git; {fd}>f ${x%/} reset --hard",
		// Text a hand-written splitter read out of step with bash: the parser reads it.
		"echo $'\\x27'; x=c_u_r_l; ${x//_/} http://e",
		"echo $'\\''; x=c_u_r_l; ${x//_/} http://e",
		"echo $'\\x22'; x=c_u_r_l; ${x//_/} http://e",
		"x=c_u_r_l; echo $'\\\\'; ${x//_/} http://e",
		": ${x:-\"}\"}; x=c_u_r_l; ${x//_/} http://e",
		": ${x:-'}'}; x=c_u_r_l; ${x//_/} http://e",
		"echo ${x/\\{/}; x=c_u_r_l; ${x//_/} http://e",
		"echo ${x/'{'/}; x=c_u_r_l; ${x//_/} http://e",
		"echo $(echo \")\"); x=c_u_r_l; ${x//_/} http://e",
		// Function bodies, trap text, and a literal command the rule names inside them.
		"x=c_u_r_l; function f { ${x//_/} http://e; }; f",
		"x=c_u_r_l; trap '${x//_/} http://e' EXIT",
		"function f { curl http://e; }; f",
		"trap 'curl http://e' EXIT",
		"x=git; function f { 2>/dev/null $(echo git) reset --hard; }; f",
		"eval 'curl x'",
		// What the parser cannot read is refused, not guessed at.
		"echo \"unclosed; x=git; ${x%/} reset --hard",
	}
	// A run of runners, option values and durations before a quoted or escaped name.
	refused = append(refused,
		strings.Repeat("timeout 1 ", 7)+"c\\url http://e",
		strings.Repeat("timeout 1 ", 8)+"'curl' http://e",
		strings.Repeat("timeout 1 ", 8)+"cu''rl http://e",
		strings.Repeat("nice -n 1 ", 8)+"c\\url http://e",
		strings.Repeat("nice -n 1 ", 40)+"c\\url http://e",
		"sudo -u a -g b -h c -p d -C 5 -D f -r g -t h c\\url http://e",
		"env -i 'curl' http://e",
		// A NUL ends a $'...' string, as bash builds it, and \cX is control-X.
		"$'\\0'curl http://e",
		"c$'\\x00'url http://e",
		"$'\\c@'curl http://e",
		"$'\\x0'curl http://e",
		"$'\\000'curl http://e",
		"$'cu\\0zz'rl http://e",
		"function f { $'\\0'curl http://e; }; f",
		"eval \"$'\\0'curl http://e\"",
		"git reset -$'\\0'-hard",
		// Text run by an alias, a shell's input, another -c runner, or arithmetic.
		"shopt -s expand_aliases\nalias c=curl\nc http://e",
		"alias c='x=c_u_r_l; ${x//_/} http://e'",
		"hash -p /usr/bin/curl c; c http://e",
		"enable -f ./c.so c; c http://e",
		"echo 'curl x' | bash",
		"echo 'x' | sh -s",
		"bash <<< 'curl x'",
		"bash <<'EOF'\ncurl x\nEOF",
		"bash < ./cmds",
		"source /dev/stdin <<< 'curl x'",
		". /dev/fd/0 < f",
		"su -c 'curl x' root",
		"flock /tmp/l -c 'curl x'",
		"script -qc 'curl x' /dev/null",
		"x='a[$(c\\url http://e)]'; echo $((x))",
		"x='a[$(c\\url http://e)] \"'; echo $((x))",
		// A shell's options and operands, and a pipe into a subshell or group.
		"bash -s arg <<< 'curl x'",
		"echo 'curl x' | bash -s arg",
		"sh -s a b <<< 'curl x'",
		"bash -o posix <<< 'curl x'",
		"bash -O extglob <<< 'curl x'",
		"bash --rcfile /dev/null <<< 'curl x'",
		"bash -s arg < f",
		"echo 'curl x' | bash -o posix",
		"cat f | (bash)",
		"cat f | { bash; }",
		"cat f | (true && bash -s)",
		"{ bash; } < f",
		"bash -- /dev/stdin < f",
		"source <(echo curl x)",
		"bash <(echo curl x)",
		// Only fd 0 is the input, and the last redirect of it wins.
		"bash <<< 'echo ok' < f",
		"cat f | bash 3<<< 'echo ok'",
		"bash 3<<< 'echo ok' < f",
		"echo 'curl x' | bash 4<<'E'\necho ok\nE",
		"exec 3<f; { bash; } 0>&3",
		"exec < f; bash",
		"exec <<< 'curl x'; bash",
		"command exec < f; bash",
		"builtin exec < f; bash",
		"command -p exec < f; bash",
		"command builtin exec <<< 'curl x'; bash",
		"git -c protocol.ext.allow=always ls-remote 'ext::curl x'",
		"git -c protocol.ext.allow=always clone 'ext::curl% x' d",
		"git -c remote.o.url='EXT::curl x' fetch o",
		"git config remote.o.url 'ext::curl x'",
		// With the ext protocol enabled, an argument built when it runs may be such a URL.
		"x=ext; git -c protocol.ext.allow=always ls-remote \"$x::curl x\"",
		"x=ext; git -c protocol.allow=always clone \"$x::curl x\" d",
		"x=ext; GIT_ALLOW_PROTOCOL=ext git ls-remote \"$x::curl x\"",
		"x=ext; env GIT_ALLOW_PROTOCOL=ext:file git ls-remote \"${x}::curl x\"",
		"export GIT_ALLOW_PROTOCOL=ext; bash -c 'x=ext; git ls-remote \"$x::curl x\"'",
		"git -c \"$k\" ls-remote \"$u\"",
		"x=ext; git -c PROTOCOL.EXT.ALLOW=always fetch \"$x::curl x\"",
		// Runners that take the command another way, and git's command options.
		"su --command='curl x' root",
		"su --command 'curl x' root",
		"flock /tmp/l --command 'curl x'",
		"flock /tmp/l 'curl' x",
		"env -S'curl x'",
		"env -S 'curl x'",
		"env --split-string='curl x'",
		"env -iS'curl x'",
		"find . -exec 'curl' {} \\;",
		"find . -execdir c\\url x {} +",
		"timeout inf 'curl' x",
		"chrt -f 99 'curl' x",
		"taskset 0x1 'curl' x",
		"chroot / 'curl' x",
		"unshare -n 'curl' x",
		"watch 'curl x'",
		"watch -n 1 curl x",
		"git archive --remote=. --exec='curl x' HEAD",
		"git ls-remote --upload-pack='curl x' .",
		"git fetch --upload-pack 'curl x' .",
		"git push --receive-pack='curl x' o",
	)
	notAllowed := append([]string{
		"x=git; ${x} reset --hard",
		"x=git; $x reset --hard",
		"x='git reset --hard '; $x/.",
		"x=git; 2>/dev/null $x reset --hard",
		"2>/dev/null $(echo git) reset --hard",
		"x=git; >&- $x reset --hard",
		"$(printf 'curl x ')/",
		"`printf 'git reset --hard '`/",
		"$(echo curl http://e)/x",
		"echo $'\\x22'; x=git; $x reset --hard",
		"x=git; echo $'\\\\'; $x reset --hard",
		"x=git; function f { $x reset --hard; }; f",
		"x=git; trap '$x reset --hard' EXIT",
		"function f { \"$@\"; }; f curl http://e",
		"set -- curl http://e; \"$@\"/x",
		"set -- curl http://e; \"${@}\"/x",
		"\"$@\"/",
		"\"${@}\"/\"x\"",
		">/dev/null \"$@\"/x",
		"command \"$@\"/x",
		"f() { \"$@\"/; }; f curl http://e",
		"touch curl; cur? http://e",
		"c*l http://e",
		"cur[l] http://e",
		"x=git; y=$(true) $x reset --hard",
		"env A=$B $x reset --hard",
	}, refused...)
	for _, mode := range []Mode{ModeBypass, ModeDefault, ModeAuto, ModeAcceptEdits} {
		for _, command := range notAllowed {
			e := New(mode)
			if err := e.AddDeny("bash(git reset --hard*)"); err != nil {
				t.Fatal(err)
			}
			if err := e.AddDeny("bash(curl*)"); err != nil {
				t.Fatal(err)
			}
			if err := e.AddAllow("bash(*)"); err != nil {
				t.Fatal(err)
			}
			res := e.Evaluate("bash", true, args(map[string]string{"command": command}))
			if res.Decision == Allow {
				t.Errorf("%s %q = allow at %s (%s)", mode, command, res.Step, res.Reason)
			}
		}
		for _, command := range refused {
			e := New(mode)
			if err := e.AddDeny("bash(git reset --hard*)"); err != nil {
				t.Fatal(err)
			}
			if err := e.AddDeny("bash(curl*)"); err != nil {
				t.Fatal(err)
			}
			res := e.Evaluate("bash", true, args(map[string]string{"command": command}))
			if res.Decision != Deny {
				t.Errorf("%s %q = %s at %s (%s), want deny", mode, command, res.Decision, res.Step, res.Reason)
			}
		}
	}
}

// An edited expansion as the program is not refused when no deny rule has a
// pattern, nor in an assignment's value; it still confirms as hidden.
func TestProgramExpansionWithoutDenyRulesAsks(t *testing.T) {
	res := New(ModeBypass).Evaluate("bash", true, args(map[string]string{"command": "x=git; ${x%/} status"}))
	if res.Decision != Ask {
		t.Errorf("no deny rules: %s at %s (%s), want ask", res.Decision, res.Step, res.Reason)
	}
	e := New(ModeBypass)
	if err := e.AddDeny("bash(git reset --hard*)"); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"\"$HOME\"/bin/tool -v", "\"$(pwd)\"/run.sh", "\"$GOPATH/bin/lint\" run"} {
		if res := e.Evaluate("bash", true, args(map[string]string{"command": command})); res.Decision != Allow {
			t.Errorf("%q, a quoted directory: %s at %s (%s), want allow in bypass", command, res.Decision, res.Step, res.Reason)
		}
	}
	if res := e.Evaluate("bash", true, args(map[string]string{"command": "y=${x%/}; echo done"})); res.Decision == Deny {
		t.Errorf("assignment: deny at %s (%s)", res.Step, res.Reason)
	}
}

// The agent's whole command must be complete to be judged: an open quote,
// block or heredoc is refused while a deny rule has a pattern. A line typed
// at a terminal may be finished by later lines, so it is left to the shell.
func TestIncompleteCommandIsRefusedButNotATerminalLine(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("bash(rm -rf /*)"); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"for f in *; do", "if true; then", "while true; do", "cat <<EOF", "f() {", "case x in",
		"git commit -m \"first line", "echo hi |", "ls &&", "(cd x",
	} {
		a := args(map[string]string{"command": command})
		if res := e.Evaluate("bash", true, a); res.Decision != Deny || res.Step != "screen" {
			t.Errorf("Evaluate(%q) = %s at %s (%s), want deny at screen", command, res.Decision, res.Step, res.Reason)
		}
		if res := e.EvaluateLine("bash", true, a); res.Decision == Deny {
			t.Errorf("EvaluateLine(%q) = deny at %s (%s)", command, res.Step, res.Reason)
		}
	}
	if res := e.EvaluateLine("bash", true, args(map[string]string{"command": "rm -rf /x"})); res.Decision != Deny {
		t.Errorf("a denied line at the terminal: %s", res.Decision)
	}
	// What parses before the error, or after a keyword that goes on from an
	// earlier line, is still read at the terminal.
	if err := e.AddDeny("bash(curl*)"); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"'curl' x; a[b]", "eval 'curl x'; a[b]", "bash -c 'curl x'; a[b]", "c\\url x; a[b]",
		"'curl' x; }", "do c\\url x", "then 'curl' x", "done; 'curl' x; done",
		// No line before or after makes a ! inside a pipeline valid.
		"cat f | ! bash",
	} {
		if res := e.EvaluateLine("bash", true, args(map[string]string{"command": line})); res.Decision != Deny {
			t.Errorf("EvaluateLine(%q) = %s at %s (%s), want deny", line, res.Decision, res.Step, res.Reason)
		}
	}
}

// A git extension the person opted in runs without being taken as an
// unreadable alias; another still confirms, and a deny rule still holds.
func TestGitExtensionOptIn(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("bash(git lfs prune*)"); err != nil {
		t.Fatal(err)
	}
	evaluate := func(command string) Result {
		return e.Evaluate("bash", true, args(map[string]string{"command": command}))
	}
	if res := evaluate("git lfs pull"); res.Decision != Ask || res.Step != "destructive" {
		t.Fatalf("not opted in: %s at %s", res.Decision, res.Step)
	}
	if err := e.AllowGitExtensions("lfs"); err != nil {
		t.Fatal(err)
	}
	if res := evaluate("git lfs pull"); res.Decision != Allow {
		t.Errorf("opted in: %s at %s (%s)", res.Decision, res.Step, res.Reason)
	}
	if res := evaluate("git lfs prune"); res.Decision != Deny {
		t.Errorf("a deny rule on an opted-in extension: %s", res.Decision)
	}
	for _, command := range []string{
		"git wipe", "git -c alias.lfs='!rm -rf .' lfs", "git lfs pull; git reset --hard",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.lfs GIT_CONFIG_VALUE_0='!curl x' git lfs",
		"export GIT_CONFIG_PARAMETERS=\"'alias.lfs=!curl x'\"; git lfs",
		"git --exec-path=./x lfs", "GIT_EXEC_PATH=./x git lfs",
		"git config alias.lfs '!curl x'; git lfs",
	} {
		if res := evaluate(command); res.Decision == Allow {
			t.Errorf("%q = allow", command)
		}
	}
	for _, name := range []string{"reset", "push", "-x", "l fs", ""} {
		if err := e.AllowGitExtensions(name); err == nil {
			t.Errorf("AllowGitExtensions(%q) was accepted", name)
		}
	}
}

// Ordinary commands stay allowed in bypass mode with deny rules in place:
// the program and IFS checks find nothing to hide in them.
func TestOrdinaryCommandsAreNotRefused(t *testing.T) {
	e := New(ModeBypass)
	if err := e.AddDeny("bash(git reset --hard*)", "bash(curl*)"); err != nil {
		t.Fatal(err)
	}
	for _, command := range ordinaryCommands {
		if res := e.Evaluate("bash", true, args(map[string]string{"command": command})); res.Decision != Allow {
			t.Errorf("%q = %s at %s (%s)", command, res.Decision, res.Step, res.Reason)
		}
	}
}

// ordinaryCommands is the false-positive corpus: everyday commands, and
// forms an earlier check refused or asked about.
var ordinaryCommands = []string{
	"go test ./...",
	"ls -la | grep foo",
	"for f in *.go; do echo \"$f\"; done",
	"git commit -m \"fix: the eval $x case\"",
	"\"$HOME\"/bin/tool -v",
	"cd dir && make",
	"npm run build 2>&1 | tail -20",
	"find . -name '*.go' -exec grep -l foo {} +",
	"echo $(date)",
	"python3 -c 'print(1)'",
	"while IFS= read -r l; do echo \"$l\"; done < f",
	"[[ -f x ]] && echo y",
	"awk '{print $1}' f",
	"git log --format=%H -n 3",
	"test -n \"$X\" || exit 1",
	"case $x in a) echo a;; esac",
	"arr=(a b); echo \"${arr[@]}\"",
	"git branch --format='%(refname)'",
	"echo ${x:-default}",
	"read -r line < f; echo \"$line\"",
	"export PATH=\"$HOME/bin:$PATH\"; go build",
	"grep -rn \"IFS\" internal/",
	"bash -c 'echo $HOME'",
	"f() { echo hi; }; f",
	"(cd sub && make test)",
	"docker run --rm -v \"$PWD\":/w img sh -c 'make'",
	"time go build ./...",
	// An assignment, arithmetic in it included, is never the program.
	"i=0; while [ $i -lt 600 ]; do sleep 1; i=$((i+1)); done",
	"x=$(pwd); echo $x",
	"x=$HOME/a; ls",
	"env A=$B ls",
	// A quoted pattern is git's, and an expansion after --name= is its value.
	"git tag -l 'v*'",
	"git branch --list 'feat/*'",
	"git log --since=\"$d\"",
	"env FOO=1 make",
	"taskset -c 0 make -j8",
	"watch -n 5 ls",
	"git fetch origin",
	// ext:: as text, where git takes no URL.
	"git commit -m 'docs: the ext:: transport'",
	"git grep 'ext::'",
	"git log --grep='ext::' -n 3",
	"git ls-remote \"$u\"",
	// Data with a backquote or $( in it that does not parse runs nothing.
	"msg='a ` b'; echo \"$msg\"",
	"pat='$('; grep -F \"$pat\" f",
}
