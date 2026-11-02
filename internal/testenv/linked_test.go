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

// isolatingMain reports whether the file declares a TestMain whose own body
// calls testenv.Main or testenv.Isolate, or clitest.Main, which does; inside
// package clitest, its own Main. A call anywhere else in the file, or a
// comment, does not count.
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
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if x, ok := fun.X.(*ast.Ident); ok {
					ip := names[x.Name]
					if ip == testenvPkg && (fun.Sel.Name == "Main" || fun.Sel.Name == "Isolate") ||
						ip == clitestPkg && fun.Sel.Name == "Main" {
						found = true
					}
				}
			case *ast.Ident:
				if pkg == clitestPkg && fun.Name == "Main" {
					found = true
				}
			}
			return !found
		})
		return found
	}
	return false
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
