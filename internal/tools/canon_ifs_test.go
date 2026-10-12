package tools

import (
	"slices"
	"strings"
	"testing"
)

// Every way of setting IFS is read: its separators beyond blanks when the text
// shows them, and unknown when it does not.
func TestIFSChanges(t *testing.T) {
	cases := []struct {
		command, seps string
		unknown       bool
	}{
		{"IFS=_; x", "_", false},
		{"IFS=\"_,\"; x", "_,", false},
		{"IFS='_'; x", "_", false},
		{"IFS=$'\\x5f'; x", "_", false},
		{"IFS=\\_; x", "_", false},
		{"IFS+=_; x", "_", false},
		{"export IFS=_", "_", false},
		{"eval 'IFS=_'", "_", false},
		{"IFS=$'\\n'; x", "", false},
		{"IFS= read -r l", "", false},
		{"IFS=, read -r a b", "", false},
		{"unset IFS; echo $IFS ${IFS} ${IFS:-x}", "", false},
		{"unset -v IFS", "", false},
		{"IFS=$s", "", true},
		{"IFS=\"a$s\"", "a", true},
		{"IFS=`x`", "", true},
		{"read IFS", "", true},
		{"printf -v IFS _", "", true},
		{"declare -n v=IFS", "", true},
		{": ${IFS:=_}", "", true},
		{": ${IFS=_}", "", true},
		{"MYIFS=_; IFSX=_", "", false},
		{"eval I\"\"FS=_", "_", false},
		{"eval I\\FS=_", "_", false},
		{"eval $'I\\x46S=_'", "_", false},
		{"eval eval 'I\\\\FS=_'", "_", false},
		{"eval $\"I\"FS=_", "_", false},
		{"sh -c 'IFS=_'", "_", false},
		{"eval ${a}FS=_", "", true},
		{"eval \"$cmd\"", "", true},
		{"eval $(echo I)FS=_", "", true},
		{"bash -c \"${a}FS=_\"", "", true},
		{"read ${v}FS", "", true},
		{"read -r \"$v\" <<< _", "", true},
		{"printf -v \"$v\" _", "", true},
		{"declare -n r=$v", "", true},
		{"local ${v}=_", "", true},
		{"for IFS in _; do :; done", "", true},
		{"x=IFS; declare -n r=$x", "", true},
		{"echo \"read IFS\"", "", true},
		{"grep IFS f; echo $x", "", false},
		{"man bash | grep -n IFS", "", false},
		{"echo IFS", "", false},
		{"export PATH=$HOME/bin:$PATH", "", false},
		{"read -r line < \"$f\"; echo $line", "", false},
		{"for f in $(ls); do echo $f; done", "", false},
		{"bash -c 'echo $HOME'", "", false},
		{"eval 'grep IFS f'", "", false},
		{"(( IFS = 1 ))", "", true},
		{"(( IFS += 1 ))", "", true},
		{"(( 0 , IFS = 1 ))", "", true},
		{"(( IFS=1+1 ))", "", true},
		{": $(( IFS = 1 ))", "", true},
		{"y=$(( IFS = 1 ))", "", true},
		{": $[ IFS = 1 ]", "", true},
		{"(( IFS++ ))", "", true},
		{"(( ++IFS ))", "", true},
		{"let IFS+=1", "", true},
		{"[ \"$a\" == IFS ]", "", false},
		{"printf -vIFS _", "", true},
		{"read -aIFS <<< _", "", true},
		{"printf -v\"${v}FS\" _", "", true},
		{"read -a \"$v\" <<< _", "", true},
		{"read 2>/dev/null ${v}FS <<< _", "", true},
		{"read -r -p \"$prompt\" line", "", false},
		{"eval \"eval \\${v}FS=_\"", "", true},
		{"bash -c \"eval \\${0}FS=_; echo\" I", "", true},
		{"git commit -m \"say eval $x\"", "", false},
		{"cut -d, -f2 f; grep -iIFS x", "", false},
	}
	for _, c := range cases {
		seps, unknown := ifsChanges(c.command)
		// An unknown value's separators are moot: the command is refused or asked.
		if unknown != c.unknown || !unknown && seps != c.seps {
			t.Errorf("%q = %q, %v; want %q, %v", c.command, seps, unknown, c.seps, c.unknown)
		}
	}
}

// A program's word with an expansion is hidden unless the expansion is plain,
// inside double quotes and followed by a /, so it can only be a directory.
func TestProgramNamedByAnExpansion(t *testing.T) {
	for command, hidden := range map[string]bool{
		"$(printf 'curl x ')/":       true,
		"\"$@\"/x":                   true,
		"\"${@}\"/x":                 true,
		"`printf 'curl x '`/":        true,
		"$(pwd)/run.sh":              true,
		"$HOME/bin/tool -v":          true,
		"$x reset --hard":            true,
		"\"$x\" reset --hard":        true,
		"\"${x%/}\"/bin/tool":        true,
		"\"$d\"/bin/$x":              true,
		"x=git; ${x%/} reset":        true,
		"\"$(pwd)\"/run.sh":          false,
		"\"$(printf 'curl x ')\"/":   false,
		"\"$HOME\"/bin/tool -v":      false,
		"\"$GOPATH/bin/x\" run":      false,
		"\"${HOME}/bin/tool\"":       false,
		"bash \"$SKILL_DIR\"/run.sh": false,
		"./bin/tool $x":              false,
		"echo $$ $?":                 false,
		"'$HOME'/bin/tool":           false,
	} {
		c := CanonicalCommand(command)
		if _, got := hiddenWords(command, c.Text); got != hidden {
			t.Errorf("%q hidden = %v, want %v", command, got, hidden)
		}
	}
}

// IFSCause says why IFS is unknown, and the destructive reason follows it: a
// command that does not parse is not said to change IFS.
func TestIFSCause(t *testing.T) {
	for command, want := range map[string]string{
		"IFS=_; $x":                   "",
		"IFS=$v; $x":                  IFSByName,
		"for IFS in a b; do $c; done": IFSByLoop,
		"for IFS in $v; do $c; done":  IFSByName,
		"((IFS=1)); $x":               IFSByArithmetic,
		"((MYIFS=1)); $x":             "",
		`eval "$x"`:                   IFSByBuilt,
		`echo "open $x`:               IFSByUnparsed,
	} {
		if got := CanonicalCommand(command).IFSCause; got != want {
			t.Errorf("%q: IFSCause %q, want %q", command, got, want)
		}
	}
	if what, _ := CanonicalCommand(`echo "open $x`).Destructive(); !strings.Contains(what, "cannot be parsed") {
		t.Errorf("unparsed command: destructive reason %q", what)
	}
}

// A word's braces are read as the words bash makes of them, at any depth,
// as bash 3.2 and later versions each read them; only an expansion past the
// budget hides its words.
func TestBracesAreReadAsTheWordsTheyMake(t *testing.T) {
	for command, want := range map[string][]string{
		"mkdir -p src/{a,b}":             {"mkdir -p src/a src/b"},
		"cp f{,.bak}":                    {"cp f f.bak"},
		"touch {a,b}.go; ls":             {"touch a.go b.go"},
		"{curl,x}":                       {"curl x"},
		"echo --x={1,2} y":               {"echo --x=1 --x=2 y"},
		"{c..c}url https://x":            {"curl https://x"},
		"c{u..u}rl https://x":            {"curl https://x"},
		"echo {a..c} {C..A}":             {"echo a b c C B A"},
		"echo {3..1} {-1..1}":            {"echo 3 2 1 -1 0 1"},
		"echo {1..5..2}":                 {"echo 1 3 5", "echo {1..5..2}"},
		"echo {01..03}":                  {"echo 01 02 03", "echo 1 2 3"},
		"echo {Z..a}":                    {"echo Z [ \\ ] ^ _ ` a"},
		"mkdir -p a/{b,c}/d{1,2}":        {"mkdir -p a/b/d1 a/b/d2 a/c/d1 a/c/d2"},
		"echo {a,{b,c}}":                 {"echo a b c"},
		"echo 'q'{a,b}":                  {"echo qa qb"},
		"echo {a..c}{1..2}":              {"echo a1 a2 b1 b2 c1 c2"},
		"bash -c 'c{u..u}rl x'":          {"curl x"},
		"eval 'git reset --{h,}ard'":     {"git reset --hard --ard"},
		"f() { {c..c}url x; }; f":        {"curl x"},
		"x=$(c{u,}rl x)":                 {"curl crl x"},
		"bash <<< 'echo {1..3..1}'":      {"echo 1 2 3", "echo {1..3..1}"},
		"for i in {1..5000}; do :; done": nil,
		"x={a,b} cmd":                    nil,
	} {
		c := CanonicalCommand(command)
		if c.Hidden {
			t.Errorf("%q: hidden", command)
		}
		for _, w := range want {
			if !slices.Contains(c.Commands(), w) {
				t.Errorf("%q: commands %q, want %q among them", command, c.Commands(), w)
			}
		}
	}
	if c := CanonicalCommand("echo {1..2000}"); c.Hidden || !slices.ContainsFunc(c.Commands(), func(s string) bool { return strings.HasSuffix(s, " 1999 2000") }) {
		t.Errorf("echo {1..2000}: hidden %v", c.Hidden)
	}
	for _, command := range []string{"echo {1..20000}", "c{u..u}rl{1..20000} x", "bash -c 'echo {1..9}{1..9}{1..9}{1..9}{1..9}'"} {
		if c := CanonicalCommand(command); !c.Hidden {
			t.Errorf("%q: not hidden", command)
		}
	}
}
