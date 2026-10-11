package toolset

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Every interface the tools package declares is accounted for here: a new
// one fails this test until it is forwarded by deferredTool, kept off
// deferral, or named as not a tool's own. Wrapping must never drop one.
func TestDeferredToolAccountsForEveryOptionalInterface(t *testing.T) {
	forwarded := map[string]bool{"Tool": true, "Hidden": true, "CallMutator": true, "Prechecker": true, "ArgResolver": true, "Targeter": true}
	notDeferred := map[string]bool{"SecretArgs": true, "ModeScoped": true, "FixedArgs": true, "StoreBound": true}
	notATools := map[string]bool{"Asker": true, "ShellHost": true}

	dir := filepath.Join("..", "tools")
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range g.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				if _, iface := ts.Type.(*ast.InterfaceType); iface {
					found = append(found, ts.Name.Name)
				}
			}
		}
	}
	sort.Strings(found)
	for _, name := range found {
		if !forwarded[name] && !notDeferred[name] && !notATools[name] {
			t.Errorf("tools.%s is not accounted for: forward it in deferredTool, or keep tools that implement it off deferral in deferrable", name)
		}
	}

	var d any = &deferredTool{Tool: fakeMCP{name: "mcp__s__x"}}
	for _, ok := range []bool{
		implements[tools.Hidden](d), implements[tools.CallMutator](d), implements[tools.Prechecker](d),
		implements[tools.ArgResolver](d), implements[tools.Targeter](d),
	} {
		if !ok {
			t.Fatal("deferredTool does not forward an interface it is said to")
		}
	}
	for _, tool := range []tools.Tool{secretArgsMCP{}, modeScopedMCP{}} {
		if deferrable(tool) {
			t.Errorf("%T would be wrapped and lose an interface", tool)
		}
	}
	if !deferrable(fakeMCP{}) {
		t.Error("a plain MCP tool is not deferred")
	}
	w := &deferredTool{Tool: targetMCP{}}
	if w.Target(nil, nil) != "host.example" || !w.MutatesCall(nil) {
		t.Error("the wrapped tool's answers are not forwarded")
	}
}

func implements[I any](v any) bool { _, ok := v.(I); return ok }

type secretArgsMCP struct{ fakeMCP }

func (secretArgsMCP) SecretArgs() []string { return []string{"token"} }

type modeScopedMCP struct{ fakeMCP }

func (modeScopedMCP) OnlyInMode() string { return "plan" }

type targetMCP struct{ fakeMCP }

func (targetMCP) Target(*tools.Session, json.RawMessage) string { return "host.example" }
func (targetMCP) MutatesCall(json.RawMessage) bool              { return true }

// A tool deferral cannot wrap safely stays offered in full.
func TestUnwrappableToolsAreNotDeferred(t *testing.T) {
	reg := tools.NewRegistry()
	for i := range 6 {
		reg.Add(fakeMCP{name: "mcp__s__t" + string(rune('a'+i))})
	}
	reg.Add(secretArgsMCP{fakeMCP{name: "mcp__s__login"}})
	if !DeferMCP(reg, 5) {
		t.Fatal("not deferred")
	}
	got, _ := reg.Get("mcp__s__login")
	if _, ok := got.(tools.SecretArgs); !ok {
		t.Fatal("the SecretArgs tool was wrapped and lost it")
	}
}
