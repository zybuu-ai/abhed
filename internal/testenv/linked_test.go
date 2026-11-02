package testenv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const managedPkg = "github.com/zybuu-ai/abhed/internal/managed"

// goList runs go list in the module's root.
func goList(t *testing.T, goos string, args ...string) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gobin, append([]string{"list"}, args...)...) // #nosec G204 -- go list of this module
	cmd.Dir = root
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GOROOT=") {
			env = append(env, e)
		}
	}
	if goos != "" {
		env = append(env, "GOOS="+goos)
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return string(out)
}

const (
	testenvPkg = "github.com/zybuu-ai/abhed/internal/testenv"
	clitestPkg = "github.com/zybuu-ai/abhed/internal/clitest"
)

// isolatingMain reports whether the file declares a TestMain whose body
// isolates before it runs anything: a statement of the body itself, not in a
// branch, a loop or a function literal, that is, or directly wraps, a call
// to testenv.Main or testenv.Isolate, or clitest.Main (inside package
// clitest, its own Main), before any statement mentions m.Run. The names are
// resolved through the file's imports, and a body that declares one of
// those names again, shadowing the package, never counts.
func isolatingMain(t *testing.T, path, pkg string) bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	names := map[string]string{} // local name -> import path
	for _, im := range f.Imports {
		ip, _ := strconv.Unquote(im.Path.Value)
		name := ip[strings.LastIndex(ip, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		names[name] = ip
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
			continue
		}
		param := ""
		if ps := fn.Type.Params.List; len(ps) == 1 && len(ps[0].Names) == 1 {
			param = ps[0].Names[0].Name
		}
		if param == "" || shadows(fn, names) {
			return false
		}
		isolating := func(e ast.Expr) bool {
			call, ok := e.(*ast.CallExpr)
			if !ok {
				return false
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				x, ok := fun.X.(*ast.Ident)
				if !ok {
					return false
				}
				ip := names[x.Name]
				return ip == testenvPkg && (fun.Sel.Name == "Main" || fun.Sel.Name == "Isolate") ||
					ip == clitestPkg && fun.Sel.Name == "Main"
			case *ast.Ident:
				return pkg == clitestPkg && fun.Name == "Main"
			}
			return false
		}
		// The call itself, or a call it is a direct argument of, as in
		// os.Exit(clitest.Main(m)).
		direct := func(e ast.Expr) bool {
			if isolating(e) {
				return true
			}
			if call, ok := e.(*ast.CallExpr); ok {
				for _, a := range call.Args {
					if isolating(a) {
						return true
					}
				}
			}
			return false
		}
		for _, st := range fn.Body.List {
			switch st := st.(type) {
			case *ast.ExprStmt:
				if direct(st.X) {
					return true
				}
			case *ast.AssignStmt:
				if len(st.Rhs) == 1 && direct(st.Rhs[0]) {
					return true
				}
			case *ast.DeclStmt:
				if gd, ok := st.Decl.(*ast.GenDecl); ok {
					for _, sp := range gd.Specs {
						if vs, ok := sp.(*ast.ValueSpec); ok && len(vs.Values) == 1 && direct(vs.Values[0]) {
							return true
						}
					}
				}
			}
			if mentionsRun(st, param) {
				return false
			}
		}
		return false
	}
	return false
}

// mentionsRun reports whether n mentions m.Run, for the TestMain parameter m.
func mentionsRun(n ast.Node, param string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == param {
				found = true
			}
		}
		return !found
	})
	return found
}

// shadows reports whether fn declares, anywhere, a name one of the file's
// imports goes by.
func shadows(fn *ast.FuncDecl, names map[string]string) bool {
	found := false
	declares := func(ids ...*ast.Ident) {
		for _, id := range ids {
			if id != nil {
				if _, ok := names[id.Name]; ok {
					found = true
				}
			}
		}
	}
	fields := func(fl *ast.FieldList) {
		if fl != nil {
			for _, f := range fl.List {
				declares(f.Names...)
			}
		}
	}
	fields(fn.Type.Params)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, l := range n.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						declares(id)
					}
				}
			}
		case *ast.ValueSpec:
			declares(n.Names...)
		case *ast.TypeSpec:
			declares(n.Name)
		case *ast.FuncLit:
			fields(n.Type.Params)
			fields(n.Type.Results)
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if id, ok := e.(*ast.Ident); ok {
						declares(id)
					}
				}
			}
		}
		return !found
	})
	return found
}

// Every test binary that links internal/managed, where every path the
// organisation's files are read from lives, runs its tests through Isolate,
// so no test reads this machine's managed files whatever it does. Nothing
// moves the paths at run time: release builds and an embedder's own test
// binaries keep reading the machine's files. The files are the ones go list
// builds into each test binary, on each operating system, so build tags
// count.
func TestEveryTestBinaryLinkingTheManagedPathsIsIsolated(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			type pkgFiles struct {
				dir   string
				files []string
			}
			pkgs := map[string]pkgFiles{}
			for _, line := range strings.Split(goList(t, goos, "-f",
				`{{.ImportPath}}|{{.Dir}}|{{join .TestGoFiles ","}},{{join .XTestGoFiles ","}}`, "./..."), "\n") {
				parts := strings.SplitN(line, "|", 3)
				if len(parts) != 3 {
					continue
				}
				var files []string
				for _, f := range strings.Split(parts[2], ",") {
					if f != "" {
						files = append(files, f)
					}
				}
				pkgs[parts[0]] = pkgFiles{parts[1], files}
			}
			var missing []string
			for _, line := range strings.Split(goList(t, goos, "-test", "-f", `{{.ImportPath}}|{{join .Deps ","}}`, "./..."), "\n") {
				ip, deps, ok := strings.Cut(line, "|")
				if !ok || !strings.HasSuffix(ip, ".test") || !slices.Contains(strings.Split(deps, ","), managedPkg) {
					continue
				}
				pkg := strings.TrimSuffix(ip, ".test")
				if pkg == testenvPkg {
					continue // the isolation itself, tested here
				}
				pf, ok := pkgs[pkg]
				if !ok {
					t.Errorf("%s: not listed", pkg)
					continue
				}
				found := false
				for _, f := range pf.files {
					if isolatingMain(t, filepath.Join(pf.dir, f), pkg) {
						found = true
						break
					}
				}
				if !found {
					missing = append(missing, strings.TrimPrefix(pkg, "github.com/zybuu-ai/abhed/"))
				}
			}
			if len(missing) > 0 {
				t.Errorf("these test binaries link internal/managed with no TestMain through internal/testenv "+
					"(add func TestMain(m *testing.M) { testenv.Main(m) }):\n  %s", strings.Join(missing, "\n  "))
			}
		})
	}
}

// The test framework stays out of what ships: a release binary and the SDK
// an embedder links do not import testing.
func TestTestingIsNotLinkedIntoReleases(t *testing.T) {
	for _, pkg := range []string{"./cmd/abhed", "./sdk"} {
		if slices.Contains(strings.Fields(goList(t, "", "-deps", pkg)), "testing") {
			t.Errorf("%s links the testing package", pkg)
		}
	}
}
