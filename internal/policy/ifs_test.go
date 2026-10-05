package policy

import "testing"

// A command that changes IFS must not get past a deny rule in any mode: the
// rules read it split on the new separators, and an expansion it may split is refused.
func TestChangedIFSCannotPassADenyRule(t *testing.T) {
	refused := []string{
		"IFS=_; git_reset_--hard",
		"IFS=_;git_reset_--hard HEAD~1",
		"IFS=_; c=git_reset_--hard; $c",
		"IFS=_; $(echo git_reset_--hard)",
		"IFS=_; `echo git_reset_--hard`",
		"IFS=_ ; x=git_push_--force; $x",
		"export IFS=_; x=git_reset_--hard; $x",
		"declare IFS=_; x=git_reset_--hard; $x",
		"IFS=_\nx=git_reset_--hard\n$x",
		"IFS=\"_\"; x=git_reset_--hard; $x",
		"IFS='_'; x=git_reset_--hard; $x",
		"IFS=$'_'; x=git_reset_--hard; $x",
		"IFS=$'\\x5f'; x=git_reset_--hard; $x",
		"IFS=\\_; x=git_reset_--hard; $x",
		"IFS+=_; x=git_reset_--hard; $x",
		"IFS=_ eval '$x'",
		"eval 'IFS=_'; x=git_reset_--hard; $x",
		"read IFS <<< _; x=git_reset_--hard; $x",
		"printf -v IFS _; x=git_reset_--hard; $x",
		"declare -n v=IFS; v=_; x=git_reset_--hard; $x",
		"unset IFS; : ${IFS:=_}; x=git_reset_--hard; $x",
		"IFS=$s; x=git_reset_--hard; $x",
		"IFS=\"$s\"; x=git_reset_--hard; $x",
		"IFS=_; x=c_u_r_l; $x",
		"(IFS=_; x=git_reset_--hard; $x)",
		"echo ok && IFS=_ && x=git_reset_--hard && $x",
		"eval I\"\"FS=_; x=git_reset_--hard; $x",
		"eval I\\FS=_; x=git_reset_--hard; $x",
		"eval $'I\\x46S=_'; x=git_reset_--hard; $x",
		"eval ${a}FS=_; x=git_reset_--hard; $x",
		"bash -c \"${a}FS=_; x=git_reset_--hard; \\$x\"",
		"read ${v}FS <<< _; x=git_reset_--hard; $x",
		"for IFS in _; do x=git_reset_--hard; $x; done",
		"IFS=_; x=git_reset_--hard; >/dev/null $x",
		"IFS=_; x=git_reset_--hard; 2>/dev/null $x",
		"IFS=_; 2>/dev/null $(echo git_reset_--hard)",
		"IFS=_; FOO=1 >/dev/null git_reset_--hard",
		"IFS=_; command >/dev/null git_reset_--hard",
		"(( IFS = 1 )); x=git1reset1--hard; $x",
		"(( IFS += 1 )); x=git1reset1--hard; $x",
		"(( 0 , IFS = 1 )); x=git1reset1--hard; $x",
		": $(( IFS = 1 )); x=git1reset1--hard; $x",
		"y=$(( IFS = 1 )); x=git1reset1--hard; $x",
		": $[ IFS = 1 ]; x=git1reset1--hard; $x",
		"printf -vIFS _; x=git_reset_--hard; $x",
		"v=I; printf -v\"${v}FS\" _; x=git_reset_--hard; $x",
		"v=I; read 2>/dev/null ${v}FS <<< _; x=git_reset_--hard; $x",
		"v=I; eval \"eval \\${v}FS=_\"; x=git_reset_--hard; $x",
		"v=I; bash -c \"eval \\${0}FS=_; x=git_reset_--hard; \\$x\" I",
		": ${x:-\"}\"}; v=I; read ${v}FS <<< _; x=git_reset_--hard; $x",
		": ${x:-\"}\"}; v=I; printf -v \"${v}FS\" _; x=git_reset_--hard; $x",
		": ${x:-\"}\"}; v=I; eval \"${v}FS=_\"; x=git_reset_--hard; $x",
		"echo $(echo \")\"); v=I; read ${v}FS <<< _; x=git_reset_--hard; $x",
		"echo ${x/\\{/}; v=I; declare \"${v}FS=_\"; x=git_reset_--hard; $x",
		"function f { IFS=_; }; f; x=git_reset_--hard; $x",
		"trap 'IFS=_' RETURN; x=git_reset_--hard; $x",
		"echo \"unclosed; IFS=_; x=git_reset_--hard; $x",
		"set -o posix; read() { :; }; IFS=_ read; x=git_reset_--hard; $x",
		"read() { :; }; IFS=_ read; x=git_reset_--hard; $x",
	}
	for _, mode := range []Mode{ModeBypass, ModeAuto, ModeAcceptEdits, ModeDefault, ModePlan} {
		for _, command := range refused {
			e := New(mode)
			if err := e.AddDeny("bash(git reset --hard*)"); err != nil {
				t.Fatal(err)
			}
			if err := e.AddAllow("bash(*)"); err != nil {
				t.Fatal(err)
			}
			res := e.Evaluate("bash", true, args(map[string]string{"command": command}))
			if res.Decision != Deny {
				t.Errorf("%s %q = %s at %s (%s), want deny", mode, command, res.Decision, res.Step, res.Reason)
			}
		}
	}
}

// IFS uses that split nothing new are not refused: blanks only, a prefix to
// read, unset IFS, reading $IFS, or a changed IFS with nothing to split.
func TestUnchangedIFSIsNotRefused(t *testing.T) {
	for _, command := range []string{
		"while IFS= read -r l; do echo $l; done < f",
		"IFS=, read -r a b <<< \"$line\"; echo \"$a\"",
		"IFS=$'\\n'; for f in $(ls); do echo \"$f\"; done",
		"unset IFS; echo $HOME",
		"IFS=_; echo hi",
		"echo posix; IFS=_ read a; echo $a",
		"grep IFS f; echo $x",
		"man bash | grep -n IFS; echo $HOME",
	} {
		e := New(ModeBypass)
		if err := e.AddDeny("bash(git reset --hard*)"); err != nil {
			t.Fatal(err)
		}
		if res := e.Evaluate("bash", true, args(map[string]string{"command": command})); res.Decision == Deny {
			t.Errorf("%q = deny at %s (%s)", command, res.Step, res.Reason)
		}
	}
}

// With no deny rule to hold, a changed IFS still confirms in bypass, read as
// the destructive command it splits into.
func TestChangedIFSIsDestructiveWithoutRules(t *testing.T) {
	for _, command := range []string{"IFS=_; git_reset_--hard", "IFS=_; c=git_reset_--hard; $c", "read IFS <<< _; $x"} {
		res := New(ModeBypass).Evaluate("bash", true, args(map[string]string{"command": command}))
		if res.Decision != Ask || res.Step != "destructive" {
			t.Errorf("%q = %s at %s (%s), want ask at destructive", command, res.Decision, res.Step, res.Reason)
		}
	}
}
