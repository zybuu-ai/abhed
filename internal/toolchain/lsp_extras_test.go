package toolchain

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func integrityOf(b []byte) string {
	s := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(s[:])
}

// fakeNPM serves pkg's metadata and tarball and pins it.
func fakeNPM(t *testing.T, f *fake, extra string, i int, pkg string, version string, files ...entry) {
	t.Helper()
	var entries []entry
	for _, e := range files {
		e.name = "package/" + e.name
		entries = append(entries, e)
	}
	tgz := tarGz(t, entries...)
	url := f.put("/npm/"+pkg+"/-/"+pkg+"-"+version+".tgz", tgz)
	f.put("/npm/"+pkg+"/"+version, []byte(fmt.Sprintf(`{"dist":{"tarball":%q,"integrity":%q}}`, url, integrityOf(tgz))))
	old := npmPins[extra]
	pins := slices.Clone(old)
	pins[i].integrity = integrityOf(tgz)
	npmPins[extra] = pins
	t.Cleanup(func() { npmPins[extra] = old })
}

// basedpyright comes from the npm registry, pinned by its integrity, runs
// on the Node.js installed beside it, and is on PATH wherever Python is.
func TestBasedpyrightFromNPM(t *testing.T) {
	f := newFake(t)
	fakeNPM(t, f, "basedpyright", 0, "basedpyright", bpyrVersion,
		entry{name: "langserver.index.js", body: "server"}, entry{name: "index.js", body: "cli"})
	src := f.source()
	src.Endpoints.NPMRegistry = f.srv.URL + "/npm/"
	c := testCache(t)
	var events []agent.ToolchainAction
	s := &Session{Cache: c, Source: src, Workspace: t.TempDir(), Platform: func() Platform { return linuxAMD64 },
		Record: func(a agent.ToolchainAction) { events = append(events, a) }}
	py := installedMarker(t, c, linuxAMD64, "python", "3.12.7")
	installedMarker(t, c, linuxAMD64, "node", "24.9.0")
	e, _ := LSPFor("python")
	ep, err := s.PlanExtra(context.Background(), e, Plan{Installed: &py})
	if err != nil || ep.Needs != nil || !strings.HasPrefix(ep.Digest, "sha512:") {
		t.Fatalf("%+v %v", ep, err)
	}
	x, err := s.InstallExtra(context.Background(), ep, py, agent.ByUser, "cli", "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := os.ReadFile(filepath.Join(x.Dir, "bin", "basedpyright-langserver"))
	if err != nil || !strings.Contains(string(w), `exec node "$d/lib/node_modules/basedpyright/langserver.index.js" "$@"`) {
		t.Fatalf("wrapper %s %v", w, err)
	}
	if !strings.Contains(s.Prelude(s.Workspace), filepath.Join(x.Dir, "bin")) {
		t.Fatal("basedpyright is not on PATH with Python")
	}
	if len(events) != 1 || events[0].Extra != "basedpyright" || events[0].Outcome != agent.ToolchainOK {
		t.Fatalf("events %+v", events)
	}
	// A registry listing another integrity than the pin is refused.
	pins := npmPins["basedpyright"]
	pins[0].integrity = integrityOf([]byte("other"))
	s.Cache = testCache(t)
	installedMarker(t, s.Cache, linuxAMD64, "node", "24.9.0")
	if _, err := s.PlanExtra(context.Background(), e, Plan{Installed: &py}); err == nil || !strings.Contains(err.Error(), "not the one Abhed pins") {
		t.Fatalf("another integrity: %v", err)
	}
}

// With no Node.js, basedpyright's plan installs it first and says so; the
// agent's tool asks for Node.js on its own instead.
func TestBasedpyrightNeedsNode(t *testing.T) {
	f := newFake(t)
	fakeNPM(t, f, "basedpyright", 0, "basedpyright", bpyrVersion, entry{name: "langserver.index.js", body: "server"}, entry{name: "index.js", body: "cli"})
	f.put("/dist/index.json", []byte(`[{"version":"v24.9.0","files":["linux-x64"],"lts":"Krypton"}]`))
	arch := tarGz(t, entry{name: "node-v24.9.0-linux-x64/bin/node", body: "node", mode: 0o755})
	f.put("/dist/v24.9.0/node-v24.9.0-linux-x64.tar.gz", arch)
	f.put("/dist/v24.9.0/SHASUMS256.txt", []byte(sum(arch)+"  node-v24.9.0-linux-x64.tar.gz\n"))
	src := f.source()
	src.Endpoints.NPMRegistry = f.srv.URL + "/npm/"
	c := testCache(t)
	var events []agent.ToolchainAction
	s := &Session{Cache: c, Source: src, Workspace: t.TempDir(), Platform: func() Platform { return linuxAMD64 },
		Record: func(a agent.ToolchainAction) { events = append(events, a) }}
	py := installedMarker(t, c, linuxAMD64, "python", "3.12.7")
	e, _ := LSPFor("python")
	ep, err := s.PlanExtra(context.Background(), e, Plan{Installed: &py})
	if err != nil || ep.Needs == nil || ep.Needs.Version() != "24.9.0" || !strings.Contains(ep.Note, "Node.js 24.9.0 is installed first") {
		t.Fatalf("%+v %v", ep, err)
	}
	if err := agentNeeds(ep); err == nil || !strings.Contains(err.Error(), `name "node" first`) {
		t.Fatalf("the agent's tool would install Node.js under Python's agreement: %v", err)
	}
	if _, err := s.InstallExtra(context.Background(), ep, py, agent.ByUser, "acp", "studio", nil); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Lang != "node" || events[1].Extra != "basedpyright" {
		t.Fatalf("events %+v", events)
	}
}

// typescript-language-server comes with the TypeScript it drives, both
// pinned, laid out so it finds TypeScript, and needs a recent Node.js.
func TestTypeScriptServerFromNPM(t *testing.T) {
	f := newFake(t)
	fakeNPM(t, f, "typescript-language-server", 0, "typescript-language-server", tslsVersion, entry{name: "lib/cli.mjs", body: "tsls"})
	fakeNPM(t, f, "typescript-language-server", 1, "typescript", tsVersion,
		entry{name: "bin/tsserver", body: "tss"}, entry{name: "bin/tsc", body: "tsc"}, entry{name: "lib/tsserver.js", body: "x"})
	src := f.source()
	src.Endpoints.NPMRegistry = f.srv.URL + "/npm/"
	c := testCache(t)
	s := &Session{Cache: c, Source: src, Workspace: t.TempDir(), Platform: func() Platform { return linuxAMD64 }}
	e, ok := LSPFor("node")
	if !ok {
		t.Fatal("no server for node")
	}
	if l, _ := Find("typescript"); l.Name != "node" {
		t.Fatalf("typescript is %q", l.Name)
	}
	if _, err := s.PlanExtra(context.Background(), e, Plan{Installed: &Installed{Version: "20.18.0"}}); err == nil || !strings.Contains(err.Error(), "22.22.2") {
		t.Fatalf("an old Node.js: %v", err)
	}
	node := installedMarker(t, c, linuxAMD64, "node", "24.9.0")
	ep, err := s.PlanExtra(context.Background(), e, Plan{Installed: &node})
	if err != nil || ep.Needs != nil {
		t.Fatalf("%+v %v", ep, err)
	}
	x, err := s.InstallExtra(context.Background(), ep, node, agent.ByUser, "cli", "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"lib/node_modules/typescript-language-server/lib/cli.mjs", "lib/node_modules/typescript/lib/tsserver.js", "bin/typescript-language-server", "bin/tsserver"} {
		if _, err := os.Stat(filepath.Join(x.Dir, filepath.FromSlash(p))); err != nil {
			t.Error(err)
		}
	}
}

// rust-analyzer is the pinned release asset, checked against its pinned
// sha256 and unpacked to one binary on PATH wherever Rust is.
func TestRustAnalyzerFromItsRelease(t *testing.T) {
	f := newFake(t)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("ELF rust-analyzer"))
	_ = zw.Close()
	f.put("/ra/"+raVersion+"/rust-analyzer-x86_64-unknown-linux-gnu.gz", gz.Bytes())
	old := raPins["linux-amd64"]
	t.Cleanup(func() { raPins["linux-amd64"] = old })
	pin := old
	pin.sha256, pin.size = sum(gz.Bytes()), int64(gz.Len())
	raPins["linux-amd64"] = pin
	src := f.source()
	src.Endpoints.RustAnalyzerDownload = f.srv.URL + "/ra/"
	c := testCache(t)
	s := &Session{Cache: c, Source: src, Workspace: t.TempDir(), Platform: func() Platform { return linuxAMD64 }}
	rust := installedMarker(t, c, linuxAMD64, "rust", "1.90.0")
	e, _ := LSPFor("rust")
	ep, err := s.PlanExtra(context.Background(), e, Plan{Installed: &rust})
	if err != nil || ep.Digest != "sha256:"+sum(gz.Bytes()) {
		t.Fatalf("%+v %v", ep, err)
	}
	x, err := s.InstallExtra(context.Background(), ep, rust, agent.ByUser, "cli", "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(x.Dir, "bin", "rust-analyzer")); string(b) != "ELF rust-analyzer" {
		t.Fatalf("binary %q", b)
	}
	if !strings.Contains(s.Prelude(s.Workspace), filepath.Join(x.Dir, "bin")) {
		t.Fatal("rust-analyzer is not on PATH with Rust")
	}
	pin.sha256 = strings.Repeat("0", 64)
	raPins["linux-amd64"] = pin
	s.Cache = testCache(t)
	installedMarker(t, s.Cache, linuxAMD64, "rust", "1.90.0")
	if _, err := s.InstallExtra(context.Background(), ep, rust, agent.ByUser, "cli", "cli", nil); err == nil {
		t.Fatal("an asset that does not match its pin was installed")
	}
	if _, err := s.PlanExtra(context.Background(), e, Plan{Installed: &rust}); err != nil {
		t.Fatal(err)
	}
	s.Platform = func() Platform { return Platform{"linux", "s390x"} }
	if _, err := s.PlanExtra(context.Background(), e, Plan{Installed: &rust}); err == nil {
		t.Fatal("rust-analyzer planned for a platform with no pinned build")
	}
}

// gopls is built by the engine with the cache's Go, as Delve is, and needs
// Go 1.26 or later to build.
func TestGoplsIsBuiltByTheEngine(t *testing.T) {
	p := Platform{OS: "linux", Arch: "arm64"}
	c := testCache(t)
	s := &Session{Cache: c, Source: &Source{Endpoints: Official}, Workspace: t.TempDir(), Platform: func() Platform { return p }}
	goIn := installedMarker(t, c, p, "go", "1.27.3")
	installedMarker(t, c, Host(), "go", "1.27.3")
	var args []string
	old := goBuild
	t.Cleanup(func() { goBuild = old })
	goBuild = func(_ context.Context, _ string, _ []string, dir string, a ...string) ([]byte, error) {
		args = a
		bin := filepath.Join(dir, "gopath", "bin")
		if Host() != p {
			bin = filepath.Join(bin, "linux_arm64")
		}
		_ = os.MkdirAll(bin, 0o755)
		return nil, os.WriteFile(filepath.Join(bin, "gopls"), []byte("ELF"), 0o755)
	}
	e, _ := LSPFor("go")
	if _, err := s.PlanExtra(context.Background(), e, Plan{Installed: &Installed{Version: "1.25.9"}}); err == nil {
		t.Fatal("gopls planned for Go 1.25")
	}
	ep, err := s.PlanExtra(context.Background(), e, Plan{Installed: &goIn})
	if err != nil {
		t.Fatal(err)
	}
	x, err := s.InstallExtra(context.Background(), ep, goIn, agent.ByUser, "cli", "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args, []string{"install", "-trimpath", "golang.org/x/tools/gopls@v" + goplsVersion}) {
		t.Fatalf("args %q", args)
	}
	if _, err := os.Stat(filepath.Join(x.Dir, "bin", "gopls")); err != nil {
		t.Fatal(err)
	}
}

// The tool installs the language server only when asked, under a scope of
// its own, and an approval for it holds only for what it named.
func TestToolInstallsTheLanguageServer(t *testing.T) {
	f := newFake(t)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("ELF"))
	_ = zw.Close()
	f.put("/ra/"+raVersion+"/rust-analyzer-x86_64-unknown-linux-gnu.gz", gz.Bytes())
	old := raPins["linux-amd64"]
	t.Cleanup(func() { raPins["linux-amd64"] = old })
	pin := old
	pin.sha256, pin.size = sum(gz.Bytes()), int64(gz.Len())
	raPins["linux-amd64"] = pin
	src := f.source()
	src.Endpoints.RustAnalyzerDownload = f.srv.URL + "/ra/"
	c := testCache(t)
	installedMarker(t, c, linuxAMD64, "rust", "1.90.0")
	s := &Session{Cache: c, Source: src, Workspace: t.TempDir(), Platform: func() Platform { return linuxAMD64 }}
	tool := Tool{S: s}
	ts, err := tools.NewSession(s.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := tool.ResolveArgs(ts, json.RawMessage(`{"name":"rust","version":"1.90","lsp":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var a toolArgs
	_ = json.Unmarshal(out, &a)
	if a.Name != "rust+lsp@1.90" || a.Resolved == nil || a.Resolved.LSP == nil || a.Resolved.LSP.Version != raVersion {
		t.Fatalf("resolved %s", out)
	}
	if tg := tool.Target(ts, out); !strings.Contains(tg, "+ rust-analyzer language server "+raVersion) {
		t.Fatalf("target %q", tg)
	}
	res := tool.Run(context.Background(), ts, out)
	if res.IsError || !strings.Contains(res.Content, "rust-analyzer language server "+raVersion+" is installed too") {
		t.Fatalf("run %+v", res)
	}
	if got := ScopeName("go", "1.27.3", true, true); got != "go+debugger+lsp@1.27" {
		t.Fatalf("scope %s", got)
	}
}

// A Rust release that ships its own rust-analyzer is used as it is,
// belonging to that Rust: nothing more is downloaded or placed, and it goes
// when that Rust is removed.
func TestRustShipsItsOwnAnalyzer(t *testing.T) {
	c := testCache(t)
	var events []agent.ToolchainAction
	s := &Session{Cache: c, Source: &Source{Endpoints: Endpoints{RustAnalyzerDownload: "https://unreachable.invalid/"}}, Workspace: t.TempDir(),
		Platform: func() Platform { return linuxAMD64 }, Record: func(a agent.ToolchainAction) { events = append(events, a) }}
	rust := installedMarker(t, c, linuxAMD64, "rust", "1.80.1")
	if err := os.WriteFile(filepath.Join(rust.Dir, "bin", "rust-analyzer"), []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	e, _ := LSPFor("rust")
	ep, err := s.PlanExtra(context.Background(), e, Plan{Installed: &rust})
	if err != nil || ep.Installed == nil || ep.Installed.Dir != rust.Dir || ep.Digest != rust.Digest || !strings.Contains(ep.Note, "nothing more is downloaded") {
		t.Fatalf("%+v %v", ep, err)
	}
	x, err := s.InstallExtra(context.Background(), ep, rust, agent.ByUser, "cli", "cli", nil)
	if err != nil || x.Dir != rust.Dir || x.Version != "rust-1.80.1" {
		t.Fatalf("%+v %v", x, err)
	}
	if len(events) != 1 || !events[0].Cached || events[0].Version != "rust-1.80.1" {
		t.Fatalf("events %+v", events)
	}
	if _, err := os.Stat(c.ExtraDir(linuxAMD64, e.Name, e.Version)); err == nil {
		t.Fatal("a folder was placed for Rust's own rust-analyzer")
	}
	if !c.ExtraAvailable(linuxAMD64, e, s.Active(s.Workspace)) {
		t.Fatal("Rust's own rust-analyzer is not counted as there")
	}
	if err := os.Chmod(rust.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(linuxAMD64, "rust", "1.80.1"); err != nil {
		t.Fatal(err)
	}
	if c.ExtraAvailable(linuxAMD64, e, s.Active(s.Workspace)) {
		t.Fatal("rust-analyzer outlived the Rust it came with")
	}
}

// For a Rust not yet installed, the offer names its own release as the
// source, and nothing else is fetched in its place should it lack one.
func TestRustAnalyzerOfferedFromTheRustToCome(t *testing.T) {
	c := testCache(t)
	s := &Session{Cache: c, Source: &Source{Endpoints: Endpoints{RustAnalyzerDownload: "https://unreachable.invalid/"}}, Workspace: t.TempDir(),
		Platform: func() Platform { return linuxAMD64 }}
	e, _ := LSPFor("rust")
	plan := Plan{Spec: Spec{Lang: "rust"}, Artifact: Artifact{Version: "1.99.1", URL: "https://static.rust-lang.org/dist/rust-1.99.1.tar.gz", Digest: "sha256:ab"}}
	ep, err := s.PlanExtra(context.Background(), e, plan)
	if err != nil || !ep.FromLanguage || ep.URL != plan.Artifact.URL || ep.Digest != "sha256:ab" || strings.Contains(ep.Source, "GitHub") {
		t.Fatalf("%+v %v", ep, err)
	}
	with := installedMarker(t, c, linuxAMD64, "rust", "1.99.1")
	if err := os.WriteFile(filepath.Join(with.Dir, "bin", "rust-analyzer"), []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	if x, err := s.InstallExtra(context.Background(), ep, with, agent.ByUser, "cli", "cli", nil); err != nil || x.Dir != with.Dir {
		t.Fatalf("%+v %v", x, err)
	}
	without := installedMarker(t, c, linuxAMD64, "rust", "1.98.0")
	if _, err := s.InstallExtra(context.Background(), ep, without, agent.ByUser, "cli", "cli", nil); err == nil || !strings.Contains(err.Error(), "carries no rust-analyzer") {
		t.Fatalf("a Rust without its own: %v", err)
	}
}
