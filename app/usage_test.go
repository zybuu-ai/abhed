package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"strconv"
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
}

// The usage table and Main's dispatch name the same subcommands, so neither
// can gain one the other lacks.
func TestUsageMatchesMainsDispatch(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	dispatched := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Main" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok || sw.Tag == nil || types.ExprString(sw.Tag) != "fs.Arg(0)" {
				return true
			}
			for _, st := range sw.Body.List {
				for _, e := range st.(*ast.CaseClause).List {
					if lit, ok := e.(*ast.BasicLit); ok {
						name, _ := strconv.Unquote(lit.Value)
						dispatched[name] = true
					}
				}
			}
			return false
		})
		return false
	})
	if len(dispatched) == 0 {
		t.Fatal("found no subcommands in Main's switch; this test would prove nothing")
	}
	for name := range dispatched {
		if !builtinCommands[name] {
			t.Errorf("Main dispatches %s, which the usage does not list", name)
		}
	}
	for _, c := range subcommands {
		if !dispatched[c.name] {
			t.Errorf("the usage lists %s, which Main does not dispatch", c.name)
		}
	}
}
