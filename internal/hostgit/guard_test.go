package hostgit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every git Abhed runs on the host goes through this package: no other
// non-test code starts git itself.
func TestNoOtherCodeRunsGit(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry holds no code to check
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			if p == filepath.Join(root, "internal", "hostgit") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p) // #nosec G304 -- this module's own source
		if err != nil {
			return nil //nolint:nilerr // as above
		}
		rel, _ := filepath.Rel(root, p)
		found, err := gitStarts(rel, src)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
		}
		for _, f := range found {
			t.Errorf("%s runs git itself; use internal/hostgit", f)
		}
		checked++
		return nil
	})
	if checked < 100 {
		t.Fatalf("only %d files were checked; the walk is not reading the module", checked)
	}
}

// The guard sees git however the call is spelled.
func TestGitStartsSeesEverySpelling(t *testing.T) {
	for name, body := range map[string]string{
		"literal":           `exec.Command("git", "status")`,
		"context":           `exec.CommandContext(ctx, "git", "status")`,
		"over lines":        "exec.CommandContext(\n\t\tctx,\n\t\t\"git\",\n\t\t\"status\",\n\t)",
		"absolute path":     `exec.Command("/usr/bin/git", "status")`,
		"local variable":    "g := \"git\"\n\texec.Command(g, \"status\")",
		"var declaration":   "var g = \"git\"\n\texec.Command(g)",
		"constant":          "exec.Command(gitBin)",
		"looked up":         "p, _ := exec.LookPath(\"git\")\n\texec.Command(p)",
		"command struct":    `_ = &exec.Cmd{Path: "/usr/bin/git"}`,
		"renamed import":    `x.Command("git")`,
		"package-level var": "exec.Command(gitVar)",
	} {
		imp := `"os/exec"`
		if name == "renamed import" {
			imp = `x "os/exec"`
		}
		src := "package p\n\nimport (\n\t\"context\"\n\t" + imp + "\n)\n\nconst gitBin = \"git\"\n\nvar gitVar = \"git\"\n\n" +
			"func f(ctx context.Context) {\n\t_ = ctx\n\t" + body + "\n}\n"
		found, err := gitStarts("p.go", []byte(src))
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, src)
		}
		if len(found) == 0 {
			t.Errorf("%s: not seen:\n%s", name, src)
		}
	}
	clean := "package p\n\nimport \"os/exec\"\n\nfunc f() {\n\t_, _ = exec.LookPath(\"git\")\n\texec.Command(\"go\", \"git\")\n}\n"
	if found, _ := gitStarts("p.go", []byte(clean)); len(found) != 0 {
		t.Errorf("an argument named git was taken for the program: %v", found)
	}
}

// gitStarts lists the places in a Go file that start git through os/exec:
// by a string naming git, or a name bound to one, such as a path LookPath
// found. LookPath alone, to see whether git is installed, starts nothing.
func gitStarts(name string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	execName := ""
	for _, im := range f.Imports {
		if p, _ := strconv.Unquote(im.Path.Value); p == "os/exec" {
			execName = "exec"
			if im.Name != nil {
				execName = im.Name.Name
			}
		}
	}
	if execName == "" || execName == "_" {
		return nil, nil
	}
	// Names bound anywhere in the file to a string naming git.
	bound := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if i < len(n.Values) && namesGit(n.Values[i], nil) {
					bound[id.Name] = true
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && i < len(n.Rhs) && namesGit(n.Rhs[i], nil) {
					bound[id.Name] = true
				}
			}
			// p, err := exec.LookPath("git") binds p to git's path.
			if len(n.Rhs) == 1 && isExecCall(n.Rhs[0], execName, "LookPath") && len(n.Lhs) > 0 {
				if call := n.Rhs[0].(*ast.CallExpr); len(call.Args) == 1 && namesGit(call.Args[0], nil) {
					if id, ok := n.Lhs[0].(*ast.Ident); ok {
						bound[id.Name] = true
					}
				}
			}
		}
		return true
	})
	var found []string
	at := func(n ast.Node) { found = append(found, fset.Position(n.Pos()).String()) }
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			prog := -1
			switch {
			case isExecCall(n, execName, "Command"):
				prog = 0
			case isExecCall(n, execName, "CommandContext"):
				prog = 1
			}
			if prog >= 0 && prog < len(n.Args) && namesGit(n.Args[prog], bound) {
				at(n)
			}
		case *ast.CompositeLit:
			if sel, ok := n.Type.(*ast.SelectorExpr); ok && isPkg(sel.X, execName) && sel.Sel.Name == "Cmd" {
				for _, el := range n.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok && isIdent(kv.Key, "Path") && namesGit(kv.Value, bound) {
						at(n)
					}
				}
			}
		}
		return true
	})
	return found, nil
}

// namesGit reports whether e is a string naming git (git, or a path ending
// in /git or git.exe), or a name bound to one.
func namesGit(e ast.Expr, bound map[string]bool) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return false
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return false
		}
		base := path.Base(strings.ReplaceAll(v, `\`, "/"))
		return base == "git" || base == "git.exe"
	case *ast.Ident:
		return bound[e.Name]
	case *ast.ParenExpr:
		return namesGit(e.X, bound)
	}
	return false
}

func isExecCall(e ast.Expr, execName, fn string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && isPkg(sel.X, execName) && sel.Sel.Name == fn
}

func isPkg(e ast.Expr, name string) bool { return isIdent(e, name) }

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
