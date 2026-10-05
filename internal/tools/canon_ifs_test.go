package tools

import "testing"

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
