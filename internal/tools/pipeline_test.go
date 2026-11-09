package tools

import (
	"slices"
	"strings"
	"testing"
)

// A plain pipeline is split into its words, quotes undone; anything a
// login shell would read as more than words is refused with why.
func TestPipelineWords(t *testing.T) {
	stages, why := PipelineWords(`du -ah '/var/log/app' | sort -rh | head -n '20'`)
	if why != "" || len(stages) != 3 || !slices.Equal(stages[0], []string{"du", "-ah", "/var/log/app"}) ||
		!slices.Equal(stages[2], []string{"head", "-n", "20"}) {
		t.Fatalf("%q %q", stages, why)
	}
	stages, why = PipelineWords(`find /var/log -name 'core*' -exec ls -lh {} \;`)
	if why != "" || !slices.Equal(stages[0][len(stages[0])-2:], []string{"{}", ";"}) {
		t.Fatalf("find: %q %q", stages, why)
	}
	for cmd, want := range map[string]string{
		"ls > x":    "redirection",
		"ls $(id)":  "expansion",
		"ls; id":    "list",
		"(ls)":      "subshell",
		"ls || id":  "||",
		"ls 'a\\b'": "backslash",
		`ls "a$b"`:  "double quote",
		`find /srv -name "x -o -fprint /home/ops/.profile -name y"`: "rc",
		"ls ~/{a,b}":               "brace",
		"ls =ls":                   "zsh",
		"ls \\x":                   "backslash",
		"ls *":                     "option",
		"FOO=1 ls":                 "variable",
		"ls 'a":                    "not closed",
		`ls "a`:                    "double quote",
		"   ":                      "empty",
		"find . -exec ls {} \\; x": "backslash",
		"ls \x7f":                  "control character",
		"ls 'a\x1bb'":              "control character",
		"ls 'a\tb'":                "control character",
	} {
		if _, why := PipelineWords(cmd); !strings.Contains(why, want) {
			t.Errorf("%q: %q, want %q", cmd, why, want)
		}
	}
}
