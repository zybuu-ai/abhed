package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// No two event types share a name: a reader switching on the type would take
// one for the other, and the record could not tell them apart.
func TestEventTypeNamesAreUnique(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, f := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok || vs.Type == nil || identName(vs.Type) != "EventType" {
				return true
			}
			for i, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok {
					continue
				}
				name, _ := strconv.Unquote(lit.Value)
				if prev, dup := seen[name]; dup {
					t.Errorf("%s and %s are both %q", prev, vs.Names[i].Name, name)
				}
				seen[name] = vs.Names[i].Name
			}
			return true
		})
	}
	if seen["mode.changed"] != "EvModeChanged" || seen["session.started"] != "EvSessionStarted" {
		t.Fatalf("the scan missed event types: %v", seen)
	}
}

func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
