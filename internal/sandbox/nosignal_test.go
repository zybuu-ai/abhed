package sandbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The code that watches listening never signals a process, and nothing
// else in this package sends a signal outside the files that signal only
// the command's own tree or group, or a container or helper Abhed started.

// watchFiles may send no signal by any route: they import neither syscall
// nor x/sys/unix, run no program but lsof, and name none of the package's
// signalling helpers.
var watchFiles = []string{"listen_ports.go", filepath.Join("..", "..", "app", "acp_serving.go")}

// signallingFiles are the files allowed to send a signal.
var signallingFiles = map[string]bool{
	"group_unix.go": true, "tree_darwin.go": true, "tree_linux.go": true, "session_darwin.go": true, "session_linux.go": true,
	"fence_linux.go": true, "container.go": true, "container_egress.go": true, "shell.go": true,
}

// signalHelpers are this package's names for sending a signal.
var signalHelpers = map[string]bool{
	"sysKill": true, "killGroup": true, "killGroupNow": true, "endTree": true, "endMarked": true, "EndLeft": true,
	"EndWithCommand": true, "stopMember": true, "signalMember": true, "treeMember": true,
}

func TestNoListenCodeSignalsAProcess(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no files to read: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f) // #nosec G304 -- this package's own sources
		if err != nil {
			t.Fatal(err)
		}
		found, err := signalFindings(f, src, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) > 0 && !signallingFiles[f] {
			t.Errorf("%s sends a signal outside the files allowed to: %v", f, found)
		}
	}
	for _, f := range watchFiles {
		src, err := os.ReadFile(f) // #nosec G304 -- this repository's own sources
		if err != nil {
			t.Fatal(err)
		}
		found, err := signalFindings(f, src, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) > 0 {
			t.Errorf("%s, which watches listening, may signal a process: %v", f, found)
		}
	}
}

// Each way around the check that a review found is caught.
func TestSignalCheckCatchesTheBypasses(t *testing.T) {
	for _, c := range []struct {
		name, body string
		general    bool // caught outside the watch files too
	}{
		{"the package's kill alias", `sysKill(1, 9)`, false},
		{"a group helper", `killGroup(1)`, false},
		{"the marked-process helper", `endMarked("x")`, false},
		{"a kill program", `_ = exec.Command("/bin/kill", "-9", "1").Run()`, false},
		{"a variable bound to kill", `k := syscall.Kill; _ = k(1, 9)`, true},
		{"the raw system call", `syscall.Syscall(syscall.SYS_KILL, 1, 9, 0)`, true},
		{"a kill program through a context", `_ = exec.CommandContext(nil, "kill", "1").Run()`, false},
		{"an exported helper from app", `sandbox.EndLeft(nil)`, false},
	} {
		src := "package sandbox\n\nimport (\n\t\"os/exec\"\n\t\"syscall\"\n)\n\nvar _ = exec.Command\nvar _ = syscall.Kill\n\nfunc bypass() {\n\t" + c.body + "\n}\n"
		strict, err := signalFindings("x.go", []byte(src), true)
		if err != nil {
			t.Fatal(c.name, err)
		}
		bodyOnly := 0
		for _, f := range strict {
			if !strings.Contains(f, "imports") && !strings.Contains(f, ":9:") && !strings.Contains(f, ":10:") {
				bodyOnly++
			}
		}
		if bodyOnly == 0 {
			t.Errorf("%s: not caught in a watch file: %v", c.name, strict)
		}
		if c.general {
			general, _ := signalFindings("x.go", []byte(src), false)
			if len(general) == 0 {
				t.Errorf("%s: not caught in the general scan", c.name)
			}
		}
	}
	// lsof itself stays allowed, and a clean file has no finding.
	clean := "package sandbox\n\nimport \"os/exec\"\n\nfunc read() { _ = exec.Command(\"/usr/sbin/lsof\", \"-nP\").Run() }\n"
	if f, err := signalFindings("x.go", []byte(clean), true); err != nil || len(f) != 0 {
		t.Fatalf("a clean file: %v %v", f, err)
	}
}

// signalFindings lists the ways src may send a signal. Everywhere: a kill or
// tgkill call with a signal other than 0, a process's Kill or Signal, the raw
// kill system call, and a call through a name bound to a kill function.
// With strict, also: importing syscall or x/sys/unix, running any program
// but lsof, and naming one of signalHelpers.
func signalFindings(name string, src []byte, strict bool) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	note := func(n ast.Node, what string) { out = append(out, fset.Position(n.Pos()).String()+": "+what) }
	if strict {
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "syscall" || p == "golang.org/x/sys/unix" {
				note(imp, "imports "+p)
			}
		}
	}
	// Names bound to a kill function: k := syscall.Kill, var k = unix.Kill.
	bound := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		bind := func(lhs []ast.Expr, rhs []ast.Expr) {
			for i, r := range rhs {
				if sel, ok := r.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Kill" || sel.Sel.Name == "Tgkill") && i < len(lhs) {
					if id, ok := lhs[i].(*ast.Ident); ok {
						bound[id.Name] = true
					}
				}
			}
		}
		switch x := n.(type) {
		case *ast.AssignStmt:
			bind(x.Lhs, x.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(x.Names))
			for i, id := range x.Names {
				lhs[i] = id
			}
			bind(lhs, x.Values)
		}
		return true
	})
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if strict && signalHelpers[x.Name] {
				note(x, "names "+x.Name)
			}
		case *ast.SelectorExpr:
			if strict && signalHelpers[x.Sel.Name] {
				note(x, "names "+x.Sel.Name)
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && bound[id.Name] && !signalZero(x) {
				note(x, "calls "+id.Name+", a kill function")
			}
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name
			if (name == "Kill" || name == "Tgkill") && len(x.Args) >= 2 && !signalZero(x) ||
				name == "Kill" && len(x.Args) == 0 || name == "Signal" && len(x.Args) == 1 && !isPkg(sel.X) {
				note(x, "sends a signal ("+name+")")
			}
			if strings.HasPrefix(name, "Syscall") || strings.HasPrefix(name, "RawSyscall") {
				for _, a := range x.Args[:min(1, len(x.Args))] {
					if s, ok := a.(*ast.SelectorExpr); ok && (s.Sel.Name == "SYS_KILL" || s.Sel.Name == "SYS_TGKILL") {
						note(x, "makes the kill system call")
					}
					if id, ok := a.(*ast.Ident); ok && (id.Name == "SYS_KILL" || id.Name == "SYS_TGKILL") {
						note(x, "makes the kill system call")
					}
				}
			}
			if strict && isExec(sel) {
				args := x.Args
				if name == "CommandContext" && len(args) > 0 {
					args = args[1:]
				}
				lit, ok := ast.Expr(nil), false
				if len(args) > 0 {
					lit, ok = args[0].(*ast.BasicLit)
				}
				if !ok || lit.(*ast.BasicLit).Value != `"/usr/sbin/lsof"` {
					note(x, "runs a program other than /usr/sbin/lsof")
				}
			}
		}
		return true
	})
	return out, nil
}

// isExec reports exec.Command or exec.CommandContext.
func isExec(sel *ast.SelectorExpr) bool {
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "exec" && (sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext")
}

// isPkg reports syscall or unix, whose Signal is a type, not a send.
func isPkg(x ast.Expr) bool {
	id, ok := x.(*ast.Ident)
	return ok && (id.Name == "syscall" || id.Name == "unix")
}

// signalZero reports a kill with signal 0, which only asks whether a
// process exists.
func signalZero(call *ast.CallExpr) bool {
	if len(call.Args) != 2 {
		return false
	}
	lit, ok := call.Args[1].(*ast.BasicLit)
	return ok && lit.Value == "0"
}
