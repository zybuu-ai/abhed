package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The code that watches listening and offers serving addresses must never
// signal a process or run a program but lsof, by any route. From its entry
// points this follows every function and method it refers to, called or
// not, in any file of any package of this module, type-checked, and fails
// on what could send a signal or start a program.

// listenEntries are where listening and serving begin: package path, the
// receiver's type name ("" for a function) and the name.
var listenEntries = [][3]string{
	{sandboxPkg, "", "sweepListeners"}, {sandboxPkg, "", "readListenersWithin"}, {sandboxPkg, "", "readListeners"},
	{sandboxPkg, "", "ListenersOn"}, {sandboxPkg, "", "ListenersReadable"}, {sandboxPkg, "", "servers"},
	{sandboxPkg, "", "ownTree"}, {sandboxPkg, "", "hostListeners"}, {sandboxPkg, "", "watchListeners"},
	{sandboxPkg, "", "unwatchListeners"}, {sandboxPkg, "", "HeldAtStart"}, {sandboxPkg, "", "SetListenWatch"},
	{sandboxPkg, "", "Listens"}, {sandboxPkg, "Process", "heldByOthers"}, {sandboxPkg, "Process", "listenable"},
	{sandboxPkg, "Process", "listenPorts"}, {sandboxPkg, "Process", "listening"},
	{appPkg, "acpConn", "watchServing"}, {appPkg, "acpConn", "offerListen"}, {appPkg, "acpConn", "sandboxListen"},
	{appPkg, "", "contestedOrUnread"}, {appPkg, "", "servesLoopback"}, {appPkg, "", "listenOffer"},
	{appPkg, "acpTerminal", "heldAtStart"}, {appPkg, "acpTerminal", "listenPorts"},
	{appPkg, "acpSession", "watchListening"}, {appPkg, "acpSession", "loadListen"},
}

const (
	modulePath = "github.com/zybuu-ai/abhed"
	sandboxPkg = modulePath + "/internal/sandbox"
	appPkg     = modulePath + "/app"
)

// reflectCalls are the reflect functions and methods that call, or find
// something to call, by name or value: what reflect can do beyond looking.
var reflectCalls = map[string]bool{"Call": true, "CallSlice": true, "Method": true, "MethodByName": true, "MakeFunc": true}

// signalHelperNames are this package's names for sending a signal.
var signalHelperNames = map[string]bool{
	"sysKill": true, "killGroup": true, "killGroupNow": true, "endTree": true, "endMarked": true, "EndLeft": true,
	"EndWithCommand": true, "stopMember": true, "signalMember": true, "treeMember": true,
}

func TestListeningReachesNoSignal(t *testing.T) {
	r, err := newReach(nil)
	if err != nil {
		t.Fatalf("the module's packages cannot be listed: %v", err)
	}
	if found := r.walk(listenEntries); len(found) > 0 {
		t.Fatalf("code reached from listening may signal a process or start a program:\n%s", strings.Join(found, "\n"))
	}
	t.Logf("%d functions followed", len(r.seen))
	if len(r.seen) < 20 {
		t.Fatalf("only %d functions were followed; the entry points moved", len(r.seen))
	}
}

// Each route a review found back to a signal, through a new file of either
// package, is caught; the clean code is not.
func TestListeningReachCatchesTheRoutes(t *testing.T) {
	sandboxDir, appDir := mustDir(t, sandboxPkg), mustDir(t, appPkg)
	hook := func(dir, file, call string) map[string]string {
		path := filepath.Join(dir, file)
		src, err := os.ReadFile(path) // #nosec G304 -- this module's own source
		if err != nil {
			t.Fatal(err)
		}
		return map[string]string{path: string(src) + "\nfunc init() { _ = " + call + " }\n"}
	}
	sandboxRoute := func(body string, imports ...string) map[string]string {
		o := hook(sandboxDir, "listen_ports.go", "reachRoute")
		o[filepath.Join(sandboxDir, "listen_reach_route.go")] = route("sandbox", body, imports)
		// The hook makes init reach reachRoute; listen through sweepListeners too.
		o[filepath.Join(sandboxDir, "listen_ports.go")] = strings.Replace(o[filepath.Join(sandboxDir, "listen_ports.go")],
			"func sweepListeners() {", "func sweepListeners() {\n\treachRoute()", 1)
		return o
	}
	appRoute := func(body string, imports ...string) map[string]string {
		o := map[string]string{}
		path := filepath.Join(appDir, "acp_serving.go")
		src, err := os.ReadFile(path) // #nosec G304 -- this module's own source
		if err != nil {
			t.Fatal(err)
		}
		o[path] = strings.Replace(string(src), "func contestedOrUnread(port int) bool {", "func contestedOrUnread(port int) bool {\n\treachRoute()", 1)
		o[filepath.Join(appDir, "acp_reach_route.go")] = route("app", body, imports)
		return o
	}
	for _, c := range []struct {
		name    string
		overlay map[string]string
	}{
		{"a helper in a new sandbox file", sandboxRoute(`killGroup(1)`)},
		{"FindProcess and Kill in a new app file", appRoute(`p, _ := os.FindProcess(1); _ = p.Kill()`, "os")},
		{"a Signal method value", appRoute(`p, _ := os.FindProcess(1); s := p.Signal; _ = s(os.Kill)`, "os")},
		{"a method expression", appRoute(`p, _ := os.FindProcess(1); _ = (*os.Process).Signal(p, os.Kill)`, "os")},
		{"Kill in a composite", appRoute(`p, _ := os.FindProcess(1); fs := []func() error{p.Kill}; _ = fs`, "os")},
		{"StartProcess", appRoute(`_, _ = os.StartProcess("/bin/kill", []string{"kill", "1"}, &os.ProcAttr{})`, "os")},
		{"an exec.Cmd composite", appRoute(`_ = (&exec.Cmd{Path: "/bin/kill"}).Run()`, "os/exec")},
		{"a Path rewrite", appRoute(`c := exec.Command("/usr/sbin/lsof"); c.Path = "/bin/kill"; _ = c.Run()`, "os/exec")},
		{"an aliased import", appRoute(`_ = x.Command("/bin/kill", "1").Run()`, `x "os/exec"`)},
		{"exec.Command in a variable", appRoute(`c := exec.Command; _ = c("/bin/kill", "1").Run()`, "os/exec")},
		{"reflect", appRoute(`v := reflect.ValueOf(&struct{}{}); _ = v.MethodByName("Kill")`, "reflect")},
		{"cgo", sandboxRoute(`C.kill(1, 9)`, "C")},
		{"syscall.Kill", sandboxRoute(`_ = syscall.Kill(1, 9)`, "syscall")},
		{"a raw system call", sandboxRoute(`syscall.Syscall(syscall.SYS_KILL, 1, 9, 0)`, "syscall")},
		{"a process from the lsof command", sandboxRoute(`c := exec.Command("/usr/sbin/lsof"); _ = c.Process.Signal(os.Kill)`, "os", "os/exec")},
		{"a process held by an interface", appRoute(`p, _ := os.FindProcess(1); var k interface{ Kill() error } = p; _ = k.Kill()`, "os")},
	} {
		r, err := newReach(c.overlay)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		found := r.walk(listenEntries)
		real := 0
		for _, f := range found {
			if strings.HasPrefix(f, loadFailed) {
				t.Errorf("%s: %s", c.name, f)
			} else {
				real++
			}
		}
		if real == 0 {
			t.Errorf("%s: not caught", c.name)
		}
		t.Logf("%s: %v", c.name, found)
	}
	// Clean code reached from the same hooks passes.
	for name, o := range map[string]map[string]string{
		"sandbox": sandboxRoute(`_ = exec.Command("/usr/sbin/lsof", "-nP")`, "os/exec"),
		"app":     appRoute(`_ = strings.ToUpper("x")`, "strings"),
	} {
		r, err := newReach(o)
		if err != nil {
			t.Fatal(name, err)
		}
		if found := r.walk(listenEntries); len(found) > 0 {
			t.Errorf("clean %s route caught: %v", name, found)
		}
	}
}

// route is a new file declaring reachRoute with body.
func route(pkg, body string, imports []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	for _, imp := range imports {
		if imp == "C" {
			b.WriteString("// #include <signal.h>\nimport \"C\"\n\n")
			continue
		}
		if strings.Contains(imp, " ") {
			name, path, _ := strings.Cut(imp, " ")
			fmt.Fprintf(&b, "import %s %s\n", name, path)
			continue
		}
		fmt.Fprintf(&b, "import %q\n", imp)
	}
	fmt.Fprintf(&b, "\nfunc reachRoute() {\n\t%s\n}\n", body)
	return b.String()
}

func mustDir(t *testing.T, pkg string) string {
	t.Helper()
	l, err := listModule()
	if err != nil {
		t.Fatalf("the module's packages cannot be listed: %v", err)
	}
	p, ok := l[pkg]
	if !ok {
		t.Fatalf("%s is not listed", pkg)
	}
	return p.Dir
}

// listedPkg is what go list says of one package.
type listedPkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	CgoFiles   []string
	Export     string
	Module     *struct{ Path string }
}

var listed struct {
	once sync.Once
	pkgs map[string]listedPkg
	err  error
}

// listModule lists the entry packages and everything they import, with the
// compiler's export data for each, once per test binary.
func listModule() (map[string]listedPkg, error) {
	listed.once.Do(func() {
		cmd := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Dir,GoFiles,CgoFiles,Export,Module", sandboxPkg, appPkg)
		cmd.Env = os.Environ()
		if goos := os.Getenv("ABHED_REACH_GOOS"); goos != "" {
			// To check another platform's files from this one.
			cmd.Env = append(cmd.Env, "GOOS="+goos)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			listed.err = fmt.Errorf("%w: %s", err, stderr.String())
			return
		}
		listed.pkgs = map[string]listedPkg{}
		dec := json.NewDecoder(bytes.NewReader(out))
		for {
			var p listedPkg
			if err := dec.Decode(&p); err == io.EOF {
				break
			} else if err != nil {
				listed.err = err
				return
			}
			listed.pkgs[p.ImportPath] = p
		}
	})
	return listed.pkgs, listed.err
}

// reach type-checks this module's packages from source as the walk enters
// them, with every import read from the compiler's export data.
type reach struct {
	fset    *token.FileSet
	pkgs    map[string]listedPkg
	overlay map[string]string
	imp     types.Importer
	src     map[string]*srcPkg
	seen    map[string]bool
}

type srcPkg struct {
	info   *types.Info
	funcs  map[string]*ast.FuncDecl
	vars   map[string]ast.Expr
	fileOf map[ast.Node]*ast.File
}

func newReach(overlay map[string]string) (*reach, error) {
	pkgs, err := listModule()
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	lookup := func(path string) (io.ReadCloser, error) {
		p, ok := pkgs[path]
		if !ok || p.Export == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(p.Export) // #nosec G304 -- the build cache's export data
	}
	return &reach{fset: fset, pkgs: pkgs, overlay: overlay, imp: importer.ForCompiler(fset, "gc", lookup),
		src: map[string]*srcPkg{}, seen: map[string]bool{}}, nil
}

// load parses and type-checks one module package from source, with the
// overlay's files in place of, or beside, the ones on disk.
func (r *reach) load(path string) (*srcPkg, error) {
	if s, ok := r.src[path]; ok {
		return s, nil
	}
	p, ok := r.pkgs[path]
	if !ok {
		return nil, fmt.Errorf("%s is not listed", path)
	}
	names := map[string]bool{}
	for _, f := range append(append([]string{}, p.GoFiles...), p.CgoFiles...) {
		names[filepath.Join(p.Dir, f)] = true
	}
	for f := range r.overlay {
		if filepath.Dir(f) == p.Dir {
			names[f] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for f := range names {
		sorted = append(sorted, f)
	}
	sort.Strings(sorted)
	var files []*ast.File
	for _, f := range sorted {
		var src any
		if o, ok := r.overlay[f]; ok {
			src = o
		}
		file, err := parser.ParseFile(r.fset, f, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	// A file that does not type-check is a failure, never a pass: what is not
	// resolved could not be followed.
	conf := types.Config{Importer: r.imp, FakeImportC: true}
	if _, err := conf.Check(path, r.fset, files, info); err != nil {
		return nil, err
	}
	s := &srcPkg{info: info, funcs: map[string]*ast.FuncDecl{}, vars: map[string]ast.Expr{}, fileOf: map[ast.Node]*ast.File{}}
	for _, file := range files {
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				s.funcs[declKey(path, d)] = d
				s.fileOf[d] = file
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for i, n := range vs.Names {
							if i < len(vs.Values) {
								s.vars[path+"."+n.Name] = vs.Values[i]
								s.fileOf[vs.Values[i]] = file
							}
						}
					}
				}
			}
		}
	}
	r.src[path] = s
	return s, nil
}

// loadFailed starts a finding for a package that could not be type-checked.
const loadFailed = "not type-checked: "

// declKey names a function declaration: package, receiver type, name.
func declKey(pkg string, d *ast.FuncDecl) string {
	recv := ""
	if d.Recv != nil && len(d.Recv.List) > 0 {
		recv = typeName(d.Recv.List[0].Type)
	}
	return pkg + "." + recv + "." + d.Name.Name
}

func typeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return typeName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return typeName(x.X)
	case *ast.IndexListExpr:
		return typeName(x.X)
	}
	return ""
}

// funcKey names a function object as declKey names its declaration.
func funcKey(f *types.Func) string {
	f = f.Origin()
	recv := ""
	if sig, ok := f.Type().(*types.Signature); ok && sig.Recv() != nil {
		t := sig.Recv().Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		if n, ok := t.(*types.Named); ok {
			recv = n.Obj().Name()
		}
	}
	if f.Pkg() == nil {
		return "." + recv + "." + f.Name()
	}
	return f.Pkg().Path() + "." + recv + "." + f.Name()
}

func inModule(pkg *types.Package) bool {
	return pkg != nil && (pkg.Path() == modulePath || strings.HasPrefix(pkg.Path(), modulePath+"/"))
}

// walk follows everything referred to from the entries and returns what
// could signal a process or start a program.
func (r *reach) walk(entries [][3]string) []string {
	var found []string
	var queue []string
	for _, e := range entries {
		queue = append(queue, e[0]+"."+e[1]+"."+e[2])
	}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if r.seen[key] {
			continue
		}
		r.seen[key] = true
		pkg := key[:strings.LastIndex(key[:strings.LastIndex(key, ".")], ".")]
		s, err := r.load(pkg)
		if err != nil {
			found = append(found, loadFailed+key+": "+err.Error())
			continue
		}
		var node ast.Node
		if d, ok := s.funcs[key]; ok {
			if d.Body == nil {
				continue
			}
			node = d
		} else if v, ok := s.vars[strings.Replace(key, "..", ".", 1)]; ok {
			node = v
		} else {
			continue
		}
		found = append(found, r.check(s, node, &queue)...)
	}
	return found
}

// check inspects one declaration, queues what it refers to in this module
// and returns what it does that could signal or start a program.
func (r *reach) check(s *srcPkg, node ast.Node, queue *[]string) []string {
	var found []string
	at := func(n ast.Node, what string) {
		found = append(found, r.fset.Position(n.Pos()).String()+": "+what)
	}
	var stack []ast.Node
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		parent, grand := ast.Node(nil), ast.Node(nil)
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		if len(stack) > 1 {
			grand = stack[len(stack)-2]
		}
		stack = append(stack, n)
		switch x := n.(type) {
		case *ast.CompositeLit:
			if isNamed(s.info.Types[x].Type, "os/exec", "Cmd") {
				at(x, "builds an exec.Cmd")
			}
		case *ast.Ident:
			obj := s.info.Uses[x]
			if obj == nil {
				return true
			}
			if pn, ok := obj.(*types.PkgName); ok {
				switch pn.Imported().Path() {
				case "C", "unsafe":
					at(x, "uses package "+pn.Imported().Path())
				}
				return true
			}
			if t := obj.Type(); t != nil && (isNamed(t, "os", "Process") || isNamed(t, "os", "ProcAttr")) {
				at(x, "holds an os.Process")
			}
			if tn, ok := obj.(*types.TypeName); ok && obj.Pkg() != nil && obj.Pkg().Path() == "os" && (tn.Name() == "Process" || tn.Name() == "ProcAttr") {
				at(x, "names os."+tn.Name())
			}
			if obj.Pkg() != nil && obj.Pkg().Path() == sandboxPkg && signalHelperNames[obj.Name()] {
				at(x, "names the signalling helper "+obj.Name())
			}
			switch o := obj.(type) {
			case *types.Var:
				if o.IsField() && (o.Name() == "Path" || o.Name() == "Args" || o.Name() == "Process" || o.Name() == "SysProcAttr") && fieldOf(s, x, "os/exec", "Cmd") {
					at(x, "touches exec.Cmd."+o.Name())
				}
				if !o.IsField() && inModule(o.Pkg()) && o.Parent() == o.Pkg().Scope() {
					*queue = append(*queue, o.Pkg().Path()+".."+o.Name())
				}
			case *types.Func:
				path := ""
				if o.Pkg() != nil {
					path = o.Pkg().Path()
				}
				name := o.Name()
				switch {
				case path == "os" && (name == "FindProcess" || name == "StartProcess"):
					at(x, "calls os."+name)
				case (path == "syscall" || path == "golang.org/x/sys/unix") &&
					(name == "Kill" || name == "Tgkill" || strings.HasPrefix(name, "Syscall") || strings.HasPrefix(name, "RawSyscall") ||
						name == "ForkExec" || name == "Exec" || name == "StartProcess"):
					at(x, "calls "+path+"."+name)
				case path == "reflect" && reflectCalls[name]:
					at(x, "calls through reflect ("+name+")")
				case path == "unsafe":
					at(x, "uses unsafe")
				case path == "os/exec" && (name == "Command" || name == "CommandContext"):
					if !lsofCall(parent, grand, x, name) {
						at(x, "runs a program other than /usr/sbin/lsof")
					}
				case path == "os" && (name == "Signal" || name == "Kill") && recvIs(o, "os", "Process"):
					at(x, "signals a process")
				case inModule(o.Pkg()):
					*queue = append(*queue, funcKey(o))
				}
			}
		}
		return true
	})
	return found
}

// lsofCall reports whether id, naming exec.Command or CommandContext, is
// the function of a call whose program is the literal /usr/sbin/lsof.
func lsofCall(parent, grand ast.Node, id *ast.Ident, name string) bool {
	var fun ast.Expr = id
	up := parent
	if sel, ok := parent.(*ast.SelectorExpr); ok && sel.Sel == id {
		fun, up = sel, grand
	}
	call, ok := up.(*ast.CallExpr)
	if !ok || call.Fun != fun {
		return false
	}
	args := call.Args
	if name == "CommandContext" && len(args) > 0 {
		args = args[1:]
	}
	if len(args) == 0 {
		return false
	}
	lit, ok := args[0].(*ast.BasicLit)
	if !ok {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && v == "/usr/sbin/lsof"
}

func isNamed(t types.Type, pkg, name string) bool {
	if t == nil {
		return false
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg && n.Obj().Name() == name
}

func recvIs(f *types.Func, pkg, name string) bool {
	sig, ok := f.Type().(*types.Signature)
	return ok && sig.Recv() != nil && isNamed(sig.Recv().Type(), pkg, name)
}

// fieldOf reports whether the field id names is one of pkg's; in os/exec the
// fields named Path, Args, Process and SysProcAttr are Cmd's.
func fieldOf(s *srcPkg, id *ast.Ident, pkg, _ string) bool {
	v, ok := s.info.Uses[id].(*types.Var)
	return ok && v.Pkg() != nil && v.Pkg().Path() == pkg
}
