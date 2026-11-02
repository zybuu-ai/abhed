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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The code that watches listening and offers serving addresses must never
// signal a process or run a program but lsof, by any route. From its entry
// points, and from every function of this module that refers to the
// listening primitives, this follows every function, method and value it
// refers to, called or not, in any file of any package of this module,
// type-checked, with the values assigned to the module's func-typed fields.
// It fails on what could send a signal or start a program, on an entry that
// no longer resolves, and on a call it cannot follow that is not allowed
// below by name.

// listenEntries are where listening and serving begin: package path, the
// receiver's type name ("" for a function) and the name.
var listenEntries = [][3]string{
	{sandboxPkg, "", "sweepListeners"}, {sandboxPkg, "", "readListenersWithin"}, {sandboxPkg, "", "readListeners"},
	{sandboxPkg, "", "ListenersOn"}, {sandboxPkg, "", "ListenersReadable"}, {sandboxPkg, "", "servers"},
	{sandboxPkg, "", "ownTree"}, {sandboxPkg, "", "hostListeners"}, {sandboxPkg, "", "watchListeners"},
	{sandboxPkg, "", "unwatchListeners"}, {sandboxPkg, "", "HeldAtStart"}, {sandboxPkg, "", "SetListenWatch"},
	{sandboxPkg, "", "Listens"}, {sandboxPkg, "Process", "heldByOthers"}, {sandboxPkg, "Process", "listenable"},
	{sandboxPkg, "Process", "listenPorts"}, {sandboxPkg, "Process", "listening"}, {sandboxPkg, "", "SetListen"},
	{sandboxPkg, "", "ListenPortsOf"}, {sandboxPkg, "", "programs"},
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

// listenPrimitives are the names whose users are entries too, so a new
// caller is checked without a list edit.
var listenPrimitives = map[string]bool{
	sandboxPkg + ".ListenersOn": true, sandboxPkg + ".readListeners": true, sandboxPkg + ".readListenersWithin": true,
	sandboxPkg + ".servers": true, sandboxPkg + ".programs": true, sandboxPkg + ".ownTree": true, sandboxPkg + ".PortContested": true,
	sandboxPkg + ".hostListeners": true, sandboxPkg + ".SetListen": true, sandboxPkg + ".Listens": true,
	sandboxPkg + ".ListenPortsOf": true, sandboxPkg + ".HeldAtStart": true, sandboxPkg + ".SetListenWatch": true,
	appPkg + ".contestedOrUnread": true,
}

// unfollowedCalls are the calls the walk cannot follow, each decided safe:
// "the function's key: the called expression".
var unfollowedCalls = map[string]string{
	// The record's redactor and store: implementations redact and write the
	// record; none signals or runs a program.
	"Redactor.Redact": "interface", "Store.Append": "interface",
	// The record's own hooks: the approval gate and the live tap, set where a
	// session is built, to decide and to stream an event.
	modulePath + "/internal/agent.Recorder.Gate": "field", modulePath + "/internal/agent.Recorder.tap": "field",
	// The owner's callbacks, which SetListenWatch stores from app's
	// watchListening, whose function literals the walk checks there.
	sandboxPkg + "..sweepListeners: *f":         "callback",
	sandboxPkg + ".Process.listening: *f":       "callback",
	sandboxPkg + ".Process.listenWatchLost: *f": "callback",
	// The change keepGrant, dropGrant and setListenAgreed pass in, function
	// literals the walk checks where they are written.
	appPkg + "..changeKeptGrants: change": "parameter",
	// A lazy sandbox resolved to its tier, which probes the tier once; it
	// signals no process and starts none of the person's.
	modulePath + "/internal/toolset..resolved: l.Resolve": "interface",
}

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
		{"a renamed entry", renamedEntry(t, appDir)},
		{"a call through an interface no one decided", appRoute(`var g interface{ Go() }; g.Go()`)},
		{"a call through a func value no one decided", appRoute(`var f func(); f()`)},
		{"a signal behind SetListen", setListenKills(t, sandboxDir)},
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

// renamedEntry renames watchServing, an entry, and its caller, and has the
// renamed function find and kill a process from a new file.
func renamedEntry(t *testing.T, appDir string) map[string]string {
	t.Helper()
	o := map[string]string{}
	for _, f := range []string{"acp_serving.go", "acp_person.go"} {
		path := filepath.Join(appDir, f)
		src, err := os.ReadFile(path) // #nosec G304 -- this module's own source
		if err != nil {
			t.Fatal(err)
		}
		o[path] = strings.ReplaceAll(string(src), "watchServing(", "watchServed(")
	}
	serving := filepath.Join(appDir, "acp_serving.go")
	o[serving] = strings.Replace(o[serving], "func (c *acpConn) watchServed(t *acpTerminal, chunk []byte) {",
		"func (c *acpConn) watchServed(t *acpTerminal, chunk []byte) {\n\treachRoute()", 1)
	o[filepath.Join(appDir, "acp_reach_route.go")] = route("app", `p, _ := os.FindProcess(1); _ = p.Kill()`, []string{"os"})
	return o
}

// setListenKills has SetListen end the sandbox's group when listening is
// turned off: the toolset's closure reaches it.
func setListenKills(t *testing.T, sandboxDir string) map[string]string {
	t.Helper()
	path := filepath.Join(sandboxDir, "process.go")
	src, err := os.ReadFile(path) // #nosec G304 -- this module's own source
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(src), "func SetListen(sb Sandbox, on bool) bool {", "func SetListen(sb Sandbox, on bool) bool {\n\tif !on {\n\t\t_ = killGroupNow(1)\n\t}", 1)
	if out == string(src) {
		t.Fatal("SetListen moved")
	}
	return map[string]string{path: out}
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
	Imports    []string
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
		cmd := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Dir,GoFiles,CgoFiles,Export,Imports,Module", sandboxPkg, appPkg)
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
	// from is what first referred to each key, to show how a finding is reached.
	from map[string]string
}

type srcPkg struct {
	path   string
	info   *types.Info
	funcs  map[string]*ast.FuncDecl
	vars   map[string]ast.Expr
	fileOf map[ast.Node]*ast.File
	// fields are the values the package assigns to func-typed fields of
	// the module's structs, by "pkg.Struct.Field".
	fields map[string][]ast.Expr
}

func newReach(overlay map[string]string) (*reach, error) {
	pkgs, err := listModule()
	if err != nil {
		return nil, err
	}
	shared.mu.Lock()
	if shared.fset == nil {
		shared.fset = token.NewFileSet()
		lookup := func(path string) (io.ReadCloser, error) {
			p, ok := pkgs[path]
			if !ok || p.Export == "" {
				return nil, fmt.Errorf("no export data for %s", path)
			}
			return os.Open(p.Export) // #nosec G304 -- the build cache's export data
		}
		shared.imp = importer.ForCompiler(shared.fset, "gc", lookup)
		shared.src = map[string]*srcPkg{}
	}
	fset, imp := shared.fset, shared.imp
	shared.mu.Unlock()
	return &reach{fset: fset, pkgs: pkgs, overlay: overlay, imp: imp, src: map[string]*srcPkg{}, seen: map[string]bool{}, from: map[string]string{}}, nil
}

// loaded are the packages type-checked so far, in a fixed order.
func (r *reach) loaded() []*srcPkg {
	paths := make([]string, 0, len(r.src))
	for p := range r.src {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]*srcPkg, 0, len(paths))
	for _, p := range paths {
		out = append(out, r.src[p])
	}
	return out
}

// shared are the packages type-checked with no overlay, kept across walks,
// with the file set and importer they were checked with.
var shared struct {
	mu   sync.Mutex
	fset *token.FileSet
	imp  types.Importer
	src  map[string]*srcPkg
}

// load parses and type-checks one module package from source, with the
// overlay's files in place of, or beside, the ones on disk.
func (r *reach) load(path string) (*srcPkg, error) {
	if s, ok := r.src[path]; ok {
		return s, nil
	}
	p, listedOK := r.pkgs[path]
	overlaid := false
	for f := range r.overlay {
		overlaid = overlaid || listedOK && filepath.Dir(f) == p.Dir
	}
	if !overlaid {
		shared.mu.Lock()
		cached, ok := shared.src[path]
		shared.mu.Unlock()
		if ok {
			r.src[path] = cached
			return cached, nil
		}
	}
	s, err := r.parseAndCheck(path)
	if err == nil && !overlaid {
		shared.mu.Lock()
		shared.src[path] = s
		shared.mu.Unlock()
	}
	return s, err
}

func (r *reach) parseAndCheck(path string) (*srcPkg, error) {
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
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{},
		Selections: map[*ast.SelectorExpr]*types.Selection{}}
	// A file that does not type-check is a failure, never a pass: what is not
	// resolved could not be followed.
	conf := types.Config{Importer: r.imp, FakeImportC: true}
	if _, err := conf.Check(path, r.fset, files, info); err != nil {
		return nil, err
	}
	s := &srcPkg{path: path, info: info, funcs: map[string]*ast.FuncDecl{}, vars: map[string]ast.Expr{}, fileOf: map[ast.Node]*ast.File{},
		fields: map[string][]ast.Expr{}}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				for _, e := range x.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							if k := funcFieldKey(info.Types[x].Type, info.Uses[id]); k != "" {
								s.fields[k] = append(s.fields[k], kv.Value)
							}
						}
					}
				}
			case *ast.AssignStmt:
				for i, l := range x.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && i < len(x.Rhs) {
						if k := selFieldKey(info, sel); k != "" {
							s.fields[k] = append(s.fields[k], x.Rhs[i])
						}
					}
				}
			}
			return true
		})
	}
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

// item is one thing to check: a declaration or value by key, or a value
// assigned to a func-typed field, checked where it was written.
type item struct {
	key  string
	pkg  *srcPkg
	node ast.Node
}

// walk follows everything referred to from the entries, and from every
// function that refers to a listening primitive, and returns what could
// signal a process or start a program, an entry that does not resolve, and
// a call it cannot follow that unfollowedCalls does not name.
func (r *reach) walk(entries [][3]string) []string {
	var found []string
	var queue []item
	for _, e := range entries {
		key := e[0] + "." + e[1] + "." + e[2]
		s, err := r.load(e[0])
		if err != nil {
			found = append(found, loadFailed+key+": "+err.Error())
			continue
		}
		if d, ok := s.funcs[key]; !ok || d.Body == nil {
			found = append(found, "the entry "+key+" does not resolve to a function with a body; update listenEntries")
			continue
		}
		queue = append(queue, item{key: key})
	}
	auto, err := r.autoEntries()
	if err != nil {
		found = append(found, loadFailed+err.Error())
	}
	queue = append(queue, auto...)
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if r.seen[it.key] {
			continue
		}
		r.seen[it.key] = true
		if it.node != nil {
			found = append(found, r.check(it.pkg, it.key, it.node, &queue)...)
			continue
		}
		pkg := it.key[:strings.LastIndex(it.key[:strings.LastIndex(it.key, ".")], ".")]
		s, err := r.load(pkg)
		if err != nil {
			found = append(found, loadFailed+it.key+": "+err.Error())
			continue
		}
		var node ast.Node
		if d, ok := s.funcs[it.key]; ok {
			if d.Body == nil {
				continue
			}
			node = d
		} else if v, ok := s.vars[strings.Replace(it.key, "..", ".", 1)]; ok {
			node = v
		} else {
			continue
		}
		found = append(found, r.check(s, it.key, node, &queue)...)
	}
	return found
}

// autoEntries are the module functions, in the sandbox package and the
// packages that import it, that refer to a listening primitive.
func (r *reach) autoEntries() ([]item, error) {
	var out []item
	for path, p := range r.pkgs {
		if path != sandboxPkg && !slices.Contains(p.Imports, sandboxPkg) || p.Module == nil || p.Module.Path != modulePath {
			continue
		}
		s, err := r.load(path)
		if err != nil {
			return out, err
		}
		for key, d := range s.funcs {
			if d.Body == nil {
				continue
			}
			// The innermost function literal around a use is the entry, else
			// the declaration: Build's closures, not all of Build.
			var lits []*ast.FuncLit
			declUses := false
			ast.Inspect(d.Body, func(n ast.Node) bool {
				if n == nil {
					return false
				}
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				o := s.info.Uses[id]
				if o == nil || o.Pkg() == nil || o.Parent() != o.Pkg().Scope() || !listenPrimitives[o.Pkg().Path()+"."+o.Name()] {
					return true
				}
				if lit := innermostLit(d.Body, id); lit != nil {
					lits = append(lits, lit)
				} else {
					declUses = true
				}
				return true
			})
			if declUses {
				out = append(out, item{key: key})
			}
			for _, lit := range lits {
				out = append(out, item{key: "closure " + r.fset.Position(lit.Pos()).String(), pkg: s, node: lit})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

// innermostLit is the function literal in body that most closely holds id.
func innermostLit(body ast.Node, id *ast.Ident) *ast.FuncLit {
	var found *ast.FuncLit
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if n.Pos() > id.Pos() || n.End() < id.End() {
			return false
		}
		if lit, ok := n.(*ast.FuncLit); ok {
			found = lit
		}
		return true
	})
	return found
}

// path is the chain of references that first reached key, entry first.
func (r *reach) path(key string) string {
	chain := []string{key}
	for i := 0; i < 40; i++ {
		f, ok := r.from[key]
		if !ok || f == "" {
			break
		}
		chain = append(chain, f)
		key = f
	}
	slices.Reverse(chain)
	return strings.Join(chain, " -> ")
}

// funcFieldKey names field obj of a composite of type t when it is a
// func-typed field of a module struct: "pkg.Struct.Field".
func funcFieldKey(t types.Type, obj types.Object) string {
	v, ok := obj.(*types.Var)
	if !ok || !v.IsField() || !isFunc(v.Type()) {
		return ""
	}
	return structKey(t, v.Name())
}

// selFieldKey names the func-typed field sel selects on a module struct.
func selFieldKey(info *types.Info, sel *ast.SelectorExpr) string {
	sn := info.Selections[sel]
	if sn == nil || sn.Kind() != types.FieldVal || !isFunc(sn.Obj().Type()) {
		return ""
	}
	return structKey(sn.Recv(), sel.Sel.Name)
}

func structKey(t types.Type, field string) string {
	for {
		p, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok || !inModule(n.Obj().Pkg()) {
		return ""
	}
	return n.Obj().Pkg().Path() + "." + n.Obj().Name() + "." + field
}

func isFunc(t types.Type) bool {
	_, ok := t.Underlying().(*types.Signature)
	return ok
}

// fieldPkgs are the packages whose structs' func-typed fields the walk
// follows into the values assigned to them: the sandbox's own, and the
// toolset's Set, whose closures app calls to reach the sandbox. A call
// through any other module func field is named in unfollowedCalls.
var fieldPkgs = map[string]bool{sandboxPkg: true, modulePath + "/internal/toolset": true}

func followsFields(key string) bool {
	return fieldPkgs[key[:strings.LastIndex(key[:strings.LastIndex(key, ".")], ".")]]
}

// fieldValues are the values the module assigns to the func-typed field
// key names, from the struct's own package and every package loaded.
func (r *reach) fieldValues(key string) []item {
	pkg := key[:strings.LastIndex(key[:strings.LastIndex(key, ".")], ".")]
	if _, err := r.load(pkg); err != nil {
		return nil
	}
	var out []item
	for _, s := range r.loaded() {
		for _, v := range s.fields[key] {
			out = append(out, item{key: "value " + r.fset.Position(v.Pos()).String(), pkg: s, node: v})
		}
	}
	return out
}

// check inspects one declaration, queues what it refers to in this module
// and returns what it does that could signal or start a program, or a call
// it cannot follow.
func (r *reach) check(s *srcPkg, where string, node ast.Node, queue *[]item) []string {
	var found []string
	at := func(n ast.Node, what string) {
		found = append(found, r.fset.Position(n.Pos()).String()+": "+what+" (reached by "+r.path(where)+")")
	}
	enqueue := func(it item) {
		if _, ok := r.from[it.key]; !ok {
			r.from[it.key] = where
		}
		*queue = append(*queue, it)
	}
	locals := localFuncs(s, node)
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
		case *ast.SelectorExpr:
			if k := selFieldKey(s.info, x); k != "" && followsFields(k) {
				vals := r.fieldValues(k)
				for _, v := range vals {
					enqueue(v)
				}
			}
		case *ast.CallExpr:
			if what := r.unfollowed(s, x, locals); what != "" {
				name := where + ": " + what
				if _, ok := unfollowedCalls[name]; !ok {
					if _, ok := unfollowedCalls[what]; !ok {
						at(x, "calls "+what+", which the walk cannot follow (in "+where+"); decide and name it in unfollowedCalls")
					}
				}
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
					enqueue(item{key: o.Pkg().Path() + ".." + o.Name()})
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
					enqueue(item{key: funcKey(o)})
				}
			}
		}
		return true
	})
	return found
}

// localFuncs are the local func variables in node that hold only function
// literals written there, which are checked in place, or what a function
// outside this module returned (a context's cancel), which cannot call
// into it.
func localFuncs(s *srcPkg, node ast.Node) map[types.Object]bool {
	ok := map[types.Object]bool{}
	bad := map[types.Object]bool{}
	note := func(id *ast.Ident, v ast.Expr) {
		o := s.info.Defs[id]
		if o == nil {
			o = s.info.Uses[id]
		}
		if o == nil {
			return
		}
		switch x := ast.Unparen(v).(type) {
		case *ast.FuncLit:
			ok[o] = true
			return
		case *ast.CallExpr:
			// What a function returns: one outside the module cannot call into
			// it, and one in the module is walked, with its literals.
			var fn *ast.Ident
			switch f := ast.Unparen(x.Fun).(type) {
			case *ast.SelectorExpr:
				fn = f.Sel
			case *ast.Ident:
				fn = f
			}
			if fn != nil {
				if _, isFunc := s.info.Uses[fn].(*types.Func); isFunc {
					ok[o] = true
					return
				}
			}
		}
		bad[o] = true
	}
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, l := range x.Lhs {
				if id, isID := l.(*ast.Ident); isID {
					if len(x.Rhs) == len(x.Lhs) {
						note(id, x.Rhs[i])
					} else if len(x.Rhs) == 1 {
						note(id, x.Rhs[0])
					}
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if i < len(x.Values) {
					note(id, x.Values[i])
				}
			}
		}
		return true
	})
	for o := range bad {
		delete(ok, o)
	}
	return ok
}

// unfollowed says what call goes to when the walk cannot follow it: a
// method of one of the module's interfaces, a func-typed field with no
// value assigned in the module, or a func value in a variable or reached
// another way. "" for a call it follows or that cannot reach the module.
func (r *reach) unfollowed(s *srcPkg, call *ast.CallExpr, locals map[types.Object]bool) string {
	fun := ast.Unparen(call.Fun)
	if tv, ok := s.info.Types[fun]; ok && (tv.IsType() || tv.IsBuiltin()) {
		return ""
	}
	switch f := fun.(type) {
	case *ast.FuncLit:
		return "" // checked in place
	case *ast.Ident:
		switch o := s.info.Uses[f].(type) {
		case *types.Func:
			return ""
		case *types.Var:
			if o.Pkg() != nil && o.Parent() == o.Pkg().Scope() && inModule(o.Pkg()) {
				return "" // its value is followed
			}
			if locals[o] {
				return "" // a literal checked in place, or what a dependency returned
			}
			return f.Name
		}
		return ""
	case *ast.SelectorExpr:
		sn := s.info.Selections[f]
		if sn == nil {
			// A qualified name: a function, or a package's variable.
			if o, ok := s.info.Uses[f.Sel].(*types.Var); ok && !inModule(o.Pkg()) {
				return types.ExprString(f)
			}
			return ""
		}
		switch sn.Kind() {
		case types.MethodVal, types.MethodExpr:
			recv := types.Unalias(sn.Recv())
			if p, ok := recv.(*types.Pointer); ok {
				recv = types.Unalias(p.Elem())
			}
			if _, ok := recv.Underlying().(*types.Interface); !ok {
				return ""
			}
			if n, ok := recv.(*types.Named); ok {
				if !inModule(n.Obj().Pkg()) {
					return "" // the standard library's or a dependency's interface
				}
				return n.Obj().Name() + "." + f.Sel.Name
			}
			return types.ExprString(f)
		case types.FieldVal:
			if k := selFieldKey(s.info, f); k != "" && followsFields(k) && len(r.fieldValues(k)) > 0 {
				return ""
			}
			if k := selFieldKey(s.info, f); k != "" {
				return k
			}
			return types.ExprString(f)
		}
	}
	return types.ExprString(fun)
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
