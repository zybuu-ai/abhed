package app

import (
	"io"
	"os"
	"strings"
	"testing"
)

// stderrOf runs Main with args and returns what it wrote to stderr.
func stderrOf(t *testing.T, args []string, opts ...Option) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	code := Main(args, opts...)
	os.Stderr = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out), code
}

// Help names every subcommand with what it does, then the flags; an unknown
// flag gets the same, and -addr reads as a flag with a value.
func TestUsageListsTheSubcommands(t *testing.T) {
	own := WithCommand("audit-export", func(string, []string) int { return 0 })
	for _, args := range [][]string{{"-h"}, {"-no-such-flag"}} {
		out, code := stderrOf(t, args, own)
		if want := map[string]int{"-h": 0, "-no-such-flag": 2}[args[0]]; code != want {
			t.Errorf("%v: exit %d, want %d", args, code, want)
		}
		if !strings.HasPrefix(out, "Usage: abhed [flags] [command [args]]") && !strings.Contains(out, "\nUsage: abhed [flags]") {
			t.Errorf("%v: no synopsis:\n%s", args, out)
		}
		for _, c := range append(subcommands, struct{ name, about string }{"audit-export", "a command of this edition"}) {
			if !strings.Contains(out, "  "+c.name+strings.Repeat(" ", max(1, 11-len(c.name)))+c.about) {
				t.Errorf("%v: %s is not listed with its description:\n%s", args, c.name, out)
			}
		}
		if cmds, flags := strings.Index(out, "Commands:"), strings.Index(out, "Flags:"); cmds < 0 || flags < cmds {
			t.Errorf("%v: commands must come before flags:\n%s", args, out)
		}
		if !strings.Contains(out, "-addr string\n") || strings.Contains(out, "-addr abhed") {
			t.Errorf("%v: -addr is not shown as a flag with a value:\n%s", args, out)
		}
	}
	for _, name := range []string{"serve", "doctor", "user", "init", "hawkeye", "migrate", "acp", "rpc", "resolve", "secret"} {
		if !builtinCommands[name] {
			t.Errorf("%s is dispatched by Main but an edition could claim it", name)
		}
	}
}
