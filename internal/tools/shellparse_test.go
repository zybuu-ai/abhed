package tools

import (
	"slices"
	"strings"
	"testing"
)

// The parser reads what a command runs, wherever it is: function bodies,
// subshells, trap, eval and -c text, and past text a hand-written splitter
// reads out of step with bash.
func TestParserFindsTheCommands(t *testing.T) {
	for command, want := range map[string]string{
		"function f { curl http://e; }; f": "curl http://e",
		"f() ( curl http://e ); f":         "curl http://e",
		"trap 'curl http://e' EXIT":        "curl http://e",
		"trap -- 'curl http://e' EXIT":     "curl http://e",
		"eval 'curl x'":                    "curl x",
		"sudo bash -c 'curl x'":            "curl x",
		"'cu'rl x":                         "curl x",
		"c\\url x":                         "curl x",
		"echo $'\\x27'; curl x":            "curl x",
		": ${x:-\"}\"}; curl x":            "curl x",
		"echo $(echo \")\"); curl x":       "curl x",
		">/dev/null curl x":                "curl x",
		"$'\\0'curl x":                     "curl x",
		"c$'\\x00'url x":                   "curl x",
		"$'\\c@'curl x":                    "curl x",
		"$'\\x0'curl x":                    "curl x",
		"$'cu\\000zz'rl x":                 "curl x",
		"function f { $'\\0'curl x; }; f":  "curl x",
		"eval \"$'\\0'curl x\"":            "curl x",
		"git reset -$'\\0'-hard":           "git reset --hard",
		"echo $'\\cA'":                     "echo \x01",
		"function f { 2>/dev/null $(which git) reset --hard; }": "git reset --hard",
	} {
		if got := parseShell(command).commands; !slices.Contains(got, want) {
			t.Errorf("%q: commands %q, want %q among them", command, got, want)
		}
	}
}

// A program named by an expansion is found by the parser: edited, or plain
// and able to split, but not a quoted directory.
func TestParserReadsTheProgram(t *testing.T) {
	for command, want := range map[string]string{
		"x=git; ${x%/} reset --hard":                      "edited",
		"x=c_u_r_l; function f { ${x//_/} http://e; }; f": "edited",
		"x=c_u_r_l; trap '${x//_/} http://e' EXIT":        "edited",
		"echo $'\\x22'; x=c_u_r_l; ${x//_/} http://e":     "edited",
		"echo ${x/\\{/}; x=c_u_r_l; ${x//_/} http://e":    "edited",
		"${a[0]} x":             "edited",
		"$x reset --hard":       "expands",
		"\"$@\"/x":              "expands",
		"\"${@}\"/x":            "expands",
		"\"$*\"/x":              "expands",
		"\"$x\" reset":          "expands",
		"\"$x\"'/'y":            "expands",
		"$(pwd)/run.sh":         "expands",
		"eval \"$c\"":           "expands",
		"\"$HOME\"/bin/tool":    "",
		"\"$(pwd)\"/run.sh":     "",
		"\"$GOPATH/bin/x\" run": "",
		"$(which git) status":   "",
		"git${IFS}status":       "",
		"go test ./...":         "",
	} {
		f := parseShell(command)
		got := map[bool]string{true: "edited"}[f.edited]
		if got == "" && f.expands {
			got = "expands"
		}
		if got != want || f.unparsed {
			t.Errorf("%q = %q (unparsed %v), want %q", command, got, f.unparsed, want)
		}
	}
}

// Every way of setting IFS the parser can see marks it set: literal values
// are its separators, anything else is unknown.
func TestParserReadsIFS(t *testing.T) {
	for command, want := range map[string]string{
		"IFS=_; x":                      "_",
		"IFS+=,; x":                     ",",
		"IFS=$'\\x5f'; x":               "_",
		"function f { IFS=_; }":         "_",
		"IFS=, read -r a b":             "",
		"IFS=$v":                        "?",
		"local IFS":                     "?",
		"declare -n r=IFS":              "?",
		"declare -n r=$v":               "?",
		"declare \"${v}FS=_\"":          "?",
		"read IFS <<< _":                "?",
		"read -r -a IFS <<< _":          "?",
		"read 2>/dev/null ${v}FS <<< _": "?",
		"printf -v IFS _":               "?",
		"printf -vIFS _":                "?",
		"printf -v\"${v}FS\" _":         "?",
		"mapfile -t IFS < f":            "?",
		"getopts ab IFS":                "?",
		"for IFS in _; do :; done":      "?",
		"coproc IFS { cat; }":           "?",
		"(( IFS = 1 ))":                 "?",
		"(( (IFS) += 1 ))":              "?",
		": $(( IFS++ ))":                "?",
		": $[ IFS = 1 ]":                "?",
		"let IFS=1":                     "?",
		"a[IFS=1]=2":                    "?",
		": ${IFS:=_}":                   "?",
		"eval \"$c\"":                   "?",
		"trap \"$c\" EXIT":              "?",
		"bash -c \"$c\"":                "?",
		"eval 'IFS=_'":                  "_",
		"v=I; eval \"eval \\${v}FS=_\"": "?",
		"grep IFS f":                    "",
		"echo \"read IFS\"":             "",
		"unset IFS":                     "",
		"(( x = 1 ))":                   "",
	} {
		f := parseShell(command)
		got := f.seps
		// Text the parser refuses, such as (( (IFS) += 1 )), is unknown too.
		if f.ifsUnknown || f.unparsed {
			got = "?"
		}
		if got != want {
			t.Errorf("%q = %q (unparsed %v), want %q", command, got, f.unparsed, want)
		}
	}
}

// What the parser cannot read is taken as hiding its words; past the size it
// reads, the command asks as the rules' own split does.
func TestParserFailsClosed(t *testing.T) {
	for _, command := range []string{"echo \"unclosed; curl x", "echo ${x", "echo $(echo", "eval 'echo \"'"} {
		if f := parseShell(command); !f.unparsed {
			t.Errorf("%q was read: %+v", command, f)
		}
		if c := CanonicalCommand(command); !c.Hidden || !c.Unparsed() {
			t.Errorf("%q: hidden %v, unparsed %v", command, c.Hidden, c.Unparsed())
		}
	}
	long := "echo " + strings.Repeat("a ", maxShellBytes)
	if f := parseShell(long); !f.tooLong || f.unparsed {
		t.Errorf("a long command: %+v", f.tooLong)
	}
	nested := "true"
	for range maxShellDepth + 2 {
		nested = "eval " + strings.ReplaceAll(strings.ReplaceAll(nested, "\\", "\\\\"), " ", "\\ ")
	}
	if f := parseShell(nested); !f.unparsed {
		t.Errorf("eval nested past the bound was read")
	}
}

// Text run another way is read when it is literal, and named as unknown
// when it is not: a shell's input, hash -p, enable -f, an alias, arithmetic.
func TestParserReadsOrNamesOtherText(t *testing.T) {
	for command, want := range map[string]string{
		"alias c=curl":                  "commands:curl",
		"bash <<< 'curl x'":             "commands:curl x",
		"bash <<'EOF'\ncurl x\nEOF":     "commands:curl x",
		"su -c 'curl x' root":           "commands:curl x",
		"flock /tmp/l -c 'curl x'":      "commands:curl x",
		"script -qc 'curl x' /dev/null": "commands:curl x",
		"x='a[$(curl x)]'; echo $((x))": "commands:curl x",
		"echo 'curl x' | bash":          "opaque",
		"echo x | sh -s":                "opaque",
		"bash < ./cmds":                 "opaque",
		"bash /dev/stdin":               "opaque",
		"source /dev/stdin <<< x":       "opaque",
		"hash -p /usr/bin/curl c":       "opaque",
		"enable -f ./c.so c":            "opaque",
		"alias c=\"$v\"":                "opaque",
		"cur? x":                        "expands",
		"c*l x":                         "expands",
		"bash":                          "",
		"bash ./script.sh":              "",
		"[ -f x ] && echo y":            "",
		"ls *.go":                       "",
		"c\\*l x":                       "",
		"c\\[l] x":                      "",
	} {
		f := parseShell(command)
		got := ""
		switch {
		case f.opaque != "":
			got = "opaque"
		case f.expands:
			got = "expands"
		}
		if strings.HasPrefix(want, "commands:") {
			if !slices.Contains(f.commands, strings.TrimPrefix(want, "commands:")) {
				t.Errorf("%q: commands %q, want %q", command, f.commands, want)
			}
			continue
		}
		if got != want || f.unparsed {
			t.Errorf("%q = %q (unparsed %v), want %q", command, got, f.unparsed, want)
		}
	}
	// A prefix to read is its own only for the builtin outside POSIX mode.
	for command, want := range map[string]string{
		"IFS=_ read a": "",
		"set -o posix; read() { :; }; IFS=_ read": "_",
		"read() { :; }; IFS=_ read":               "_",
	} {
		if got := parseShell(command).seps; got != want {
			t.Errorf("%q: seps %q, want %q", command, got, want)
		}
	}
}

// $'...' decodes as bash 3.2 does, byte for byte, at the edges of \c.
func TestAnsiCMatchesBash(t *testing.T) {
	for in, want := range map[string]string{
		`a\c'`:   "a\\",
		`\c\\'`:  "\x1c\\",
		`\c?'`:   "\x1f",
		`\cA'`:   "\x01",
		`\cz'`:   "\x1a",
		`\c@x'`:  "",
		`a\0bc'`: "a",
		`\x41'`:  "A",
	} {
		if got, _ := ansiC(in, 0); got != want {
			t.Errorf("ansiC(%q) = %q, want %q", in, got, want)
		}
	}
}
