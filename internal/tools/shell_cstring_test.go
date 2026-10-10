package tools

import (
	"slices"
	"testing"
)

// A shell given -c runs the first operand after its options and "--", and
// find runs its -exec command as a command of its own: a program named by
// an expansion in either is seen.
func TestShellCommandStringAndFindExecAreRead(t *testing.T) {
	for _, c := range []string{
		`sh -c -- 'c=wget; $c https://x'`,
		`sh -c -x 'c=wget; $c https://x'`,
		`sh -xc -- 'c=wget; $c https://x'`,
		`bash -c -o pipefail 'c=wget; $c https://x'`,
		`sh -c - '$(echo wget) https://x'`,
		`busybox sh -c 'c=wget; $c https://x'`,
		`busybox ash -c -- 'c=wget; $c https://x'`,
		`/bin/busybox bash -xc '$(echo wget) https://x'`,
		`su -c -- 'c=wget; $c https://x'`,
		`su root -c -x 'c=wget; $c https://x'`,
		`flock /tmp/l -c -- 'c=wget; $c https://x'`,
		`script -q -c -- 'c=wget; $c https://x' /dev/null`,
		`find . -exec sh -c 'c=wget; $c https://x' \;`,
		`find . -execdir sh -c '${c:-wget} https://x' \;`,
		`find . -ok sh -c -- '$(echo wget) https://x' {} +`,
	} {
		if why := CanonicalCommand(c).Unreadable(); why == "" {
			t.Errorf("%s: read as readable", c)
		}
	}
	for c, want := range map[string]string{
		`sh -c -- 'ls -la'`:              "ls -la",
		`sh -xc -o pipefail 'make x'`:    "make x",
		`busybox wget https://x`:         "wget https://x",
		`busybox sh -c 'go vet'`:         "go vet",
		`find . -exec sh -c 'go vet' \;`: "go vet",
	} {
		canon := CanonicalCommand(c)
		if why := canon.Unreadable(); why != "" || !slices.Contains(canon.Commands(), want) {
			t.Errorf("%s: %q, commands %q", c, why, canon.Commands())
		}
	}
	for _, c := range []struct {
		words []string
		lits  []bool
		want  int
	}{
		{[]string{"-c", "x"}, []bool{true, true}, 1},
		{[]string{"-c", "--", "x"}, []bool{true, true, true}, 2},
		{[]string{"-co", "pipefail", "-x", "--", "x"}, []bool{true, true, true, true, true}, 4},
		{[]string{"-c", "$X"}, []bool{true, false}, 1},
		{[]string{"-c", "--"}, []bool{true, true}, -1},
	} {
		if got := ShellCommandString(c.words, c.lits); got != c.want {
			t.Errorf("%q: %d, want %d", c.words, got, c.want)
		}
	}
}
