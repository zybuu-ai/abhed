package toolchain

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// installedMarker puts a toolchain in the cache as if it were installed.
func installedMarker(t *testing.T, c Cache, p Platform, lang, version string) Installed {
	t.Helper()
	dir := c.Dir(p, lang, version)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	in := Installed{Lang: lang, Version: version, Platform: p.String(), URL: "https://example.test/" + lang, Digest: "sha256:00"}
	b, _ := json.Marshal(in)
	if err := os.WriteFile(filepath.Join(dir, markerFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	in.Dir = dir
	return in
}

func wheel(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"debugpy/__init__.py": "x = 1\n", "debugpy-1.8.22.dist-info/METADATA": "Name: debugpy\n"} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	return buf.Bytes()
}

// debugpy comes from PyPI's listing, only when the listing's sha256 is the
// pinned one, and is then on PYTHONPATH wherever Python is.
func TestDebugpyIsPinnedAndFoundByPython(t *testing.T) {
	f := newFake(t)
	whl := wheel(t)
	old := debugpyWheelSHA256
	t.Cleanup(func() { debugpyWheelSHA256 = old })
	debugpyWheelSHA256 = sum(whl)
	wurl := f.put("/packages/"+debugpyWheel, whl)
	f.put("/pypi/debugpy/1.8.22/json", []byte(fmt.Sprintf(`{"urls":[{"filename":%q,"url":%q,"size":%d,"digests":{"sha256":%q}}]}`,
		debugpyWheel, wurl, len(whl), sum(whl))))
	src := f.source()
	src.Endpoints.PyPIAPI = f.srv.URL + "/pypi/"
	c := testCache(t)
	var events []agent.ToolchainAction
	s := &Session{Cache: c, Source: src, Workspace: t.TempDir(), Platform: func() Platform { return linuxAMD64 },
		Record: func(a agent.ToolchainAction) { events = append(events, a) }}
	py := installedMarker(t, c, linuxAMD64, "python", "3.12.7")
	e, _ := DebuggerFor("python")

	if _, err := s.PlanExtra(context.Background(), e, Plan{Installed: &Installed{Version: "3.9.18"}}); err == nil || !strings.Contains(err.Error(), "3.10") {
		t.Fatalf("debugpy for Python 3.9: %v", err)
	}
	ep, err := s.PlanExtra(context.Background(), e, Plan{Installed: &py})
	if err != nil || ep.URL != wurl || ep.Digest != "sha256:"+sum(whl) || ep.Installed != nil {
		t.Fatalf("%+v %v", ep, err)
	}
	x, err := s.InstallExtra(context.Background(), ep, py, agent.ByUser, "cli", "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(x.Dir, "site", "debugpy", "__init__.py")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(x.Dir, "site", "debugpy", "planted.py"), nil, 0o644); err == nil {
		t.Fatal("the installed debugpy is writable")
	}
	if len(events) != 1 || events[0].Extra != "debugpy" || events[0].Lang != "python" || events[0].Outcome != agent.ToolchainOK {
		t.Fatalf("events %+v", events)
	}
	pre := s.Prelude(s.Workspace)
	if !strings.Contains(pre, "export PYTHONPATH="+shellQuote(filepath.Join(x.Dir, "site"))+`"${PYTHONPATH:+:$PYTHONPATH}"`) {
		t.Fatalf("prelude %s", pre)
	}
	if again, _ := s.PlanExtra(context.Background(), e, Plan{Installed: &py}); again.Installed == nil {
		t.Fatal("an installed debugpy is planned again")
	}

	// A listing whose sha256 is not the pin is refused.
	debugpyWheelSHA256 = strings.Repeat("0", 64)
	s.Cache = testCache(t)
	if _, err := s.PlanExtra(context.Background(), e, Plan{Installed: &py}); err == nil || !strings.Contains(err.Error(), "not the one Abhed pins") {
		t.Fatalf("a listing that is not the pin: %v", err)
	}
}

// Delve is built with the cache's own Go for the sandbox's platform, from
// the module proxy checked by the checksum database, with none of the
// person's Go settings, then is on PATH wherever Go is.
func TestDelveIsBuiltByTheEngine(t *testing.T) {
	t.Setenv("GOPRIVATE", "*")
	t.Setenv("GOFLAGS", "-insecure")
	p := Platform{OS: "linux", Arch: "arm64"}
	c := testCache(t)
	var events []agent.ToolchainAction
	s := &Session{Cache: c, Source: &Source{Endpoints: Official}, Workspace: t.TempDir(), Platform: func() Platform { return p },
		Record: func(a agent.ToolchainAction) { events = append(events, a) }}
	goIn := installedMarker(t, c, p, "go", "1.26.3")
	host := installedMarker(t, c, Host(), "go", "1.26.3")
	var env, args []string
	old := goBuild
	t.Cleanup(func() { goBuild = old })
	goBuild = func(_ context.Context, goBin string, e []string, dir string, a ...string) ([]byte, error) {
		env, args = e, a
		if goBin != filepath.Join(host.Dir, "bin", "go") {
			return nil, fmt.Errorf("built with %s", goBin)
		}
		bin := filepath.Join(dir, "gopath", "bin")
		if Host() != p {
			bin = filepath.Join(bin, "linux_arm64")
		}
		_ = os.MkdirAll(bin, 0o755)
		return nil, os.WriteFile(filepath.Join(bin, "dlv"), []byte("ELF"), 0o755)
	}
	e, _ := DebuggerFor("go")
	if _, err := s.PlanExtra(context.Background(), e, Plan{Installed: &Installed{Version: "1.24.9"}}); err == nil {
		t.Fatal("Delve planned for Go 1.24")
	}
	ep, err := s.PlanExtra(context.Background(), e, Plan{Installed: &goIn})
	if err != nil || !strings.Contains(ep.Note, "built here with Go 1.26.3 for linux-arm64") {
		t.Fatalf("%+v %v", ep, err)
	}
	x, err := s.InstallExtra(context.Background(), ep, goIn, "agent", "tool", "asked", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args, []string{"install", "-trimpath", "github.com/go-delve/delve/cmd/dlv@v" + delveVersion}) {
		t.Fatalf("args %q", args)
	}
	for _, want := range []string{"GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local",
		"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64", "GOPRIVATE=", "GONOSUMDB=", "GOINSECURE=", "GOENV=off"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s: %q", want, env)
		}
	}
	for _, kv := range env {
		if kv == "GOPRIVATE=*" || kv == "GOFLAGS=-insecure" {
			t.Errorf("the person's Go setting reached the build: %s", kv)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(x.Dir, "bin", "dlv")); string(b) != "ELF" || x.Digest != "sha256:"+sum([]byte("ELF")) {
		t.Fatalf("dlv %q %s", b, x.Digest)
	}
	if len(events) != 1 || events[0].Extra != "delve" || events[0].BuiltWith != "go 1.26.3" {
		t.Fatalf("events %+v", events)
	}
	if pre := s.Prelude(s.Workspace); !strings.Contains(pre, filepath.Join(x.Dir, "bin")) {
		t.Fatalf("prelude %s", pre)
	}
	// Delve is for Linux and macOS (the macOS process tier debugs natively),
	// not other systems.
	s.Platform = func() Platform { return Platform{OS: "windows", Arch: "amd64"} }
	if _, err := s.PlanExtra(context.Background(), e, Plan{Installed: &goIn}); err == nil {
		t.Fatal("Delve planned for Windows")
	}
}

// An opt-in build against the real sources: ABHED_TOOLCHAIN_LIVE=1 installs
// Go for this host, builds Delve for Linux with it, and installs debugpy.
func TestLiveDebuggers(t *testing.T) {
	if os.Getenv("ABHED_TOOLCHAIN_LIVE") != "1" {
		t.Skip("set ABHED_TOOLCHAIN_LIVE=1 to build the debuggers from the real sources")
	}
	src := &Source{Endpoints: Official, UserAgent: "abhed-test"}
	c := testCache(t)
	p := Platform{OS: "linux", Arch: Host().Arch}
	s := &Session{Cache: c, Source: src, Workspace: t.TempDir(), Platform: func() Platform { return p }}
	ctx := context.Background()
	goPlan, err := s.Plan(ctx, Spec{Lang: "go"})
	if err != nil {
		t.Fatal(err)
	}
	goIn, err := s.Install(ctx, goPlan, agent.ByUser, "cli", "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := DebuggerFor("go")
	ep, err := s.PlanExtra(ctx, e, goPlan)
	if err != nil {
		t.Fatal(err)
	}
	x, err := s.InstallExtra(ctx, ep, goIn, agent.ByUser, "cli", "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("file", filepath.Join(x.Dir, "bin", "dlv")).Output()
	t.Logf("dlv %s %s: %s", x.Digest, x.BuiltWith, out)
	if !strings.Contains(string(out), "ELF") {
		t.Fatalf("dlv is not a Linux build: %s", out)
	}
	pe, _ := DebuggerFor("python")
	pp, err := s.PlanExtra(ctx, pe, Plan{Installed: &Installed{Version: "3.12.7"}})
	if err != nil {
		t.Fatal(err)
	}
	px, err := s.InstallExtra(ctx, pp, Installed{}, agent.ByUser, "cli", "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(px.Dir, "site", "debugpy", "adapter")); err != nil {
		t.Fatal(err)
	}
}

// The tool installs the debugger only when asked, under a scope of its own,
// and only what was approved.
func TestToolInstallsTheDebuggerWithTheLanguage(t *testing.T) {
	f := newFake(t)
	plats := []Platform{linuxAMD64}
	if Host() != linuxAMD64 {
		plats = append(plats, Host())
	}
	f.goRelease("1.26.3", true, goArchive(t, "1.26.3"), plats...)
	s := &Session{Cache: testCache(t), Source: f.source(), Workspace: t.TempDir(), Platform: func() Platform { return linuxAMD64 }}
	old := goBuild
	t.Cleanup(func() { goBuild = old })
	goBuild = func(_ context.Context, _ string, _ []string, dir string, _ ...string) ([]byte, error) {
		bin := filepath.Join(dir, "gopath", "bin")
		if Host() != linuxAMD64 {
			bin = filepath.Join(bin, "linux_amd64")
		}
		_ = os.MkdirAll(bin, 0o755)
		return nil, os.WriteFile(filepath.Join(bin, "dlv"), []byte("ELF"), 0o755)
	}
	tool := Tool{S: s}
	ts, err := tools.NewSession(s.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := tool.ResolveArgs(ts, json.RawMessage(`{"name":"go","debugger":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var a toolArgs
	_ = json.Unmarshal(out, &a)
	if a.Name != "go+debugger@1.26" || a.Resolved == nil || a.Resolved.Debugger == nil || a.Resolved.Debugger.Version != delveVersion {
		t.Fatalf("resolved %s", out)
	}
	if tg := tool.Target(ts, out); !strings.Contains(tg, "Go 1.26.3") || !strings.Contains(tg, "+ Delve debugger "+delveVersion) {
		t.Fatalf("target %q", tg)
	}
	stale := strings.Replace(string(out), `"version":"`+delveVersion+`"`, `"version":"1.0.0"`, 1)
	if res := tool.Run(context.Background(), ts, json.RawMessage(stale)); !res.IsError || !strings.Contains(res.Content, "changed since it was approved") {
		t.Fatalf("stale %+v", res)
	}
	res := tool.Run(context.Background(), ts, out)
	if res.IsError || !strings.Contains(res.Content, "Delve debugger "+delveVersion+" is installed too") {
		t.Fatalf("run %+v", res)
	}
	var extras int
	for _, e := range res.Events {
		if a, ok := e.Payload.(agent.ToolchainAction); ok && a.Extra == "delve" && a.Outcome == agent.ToolchainOK {
			extras++
		}
	}
	if extras != 1 {
		t.Fatalf("events %+v", res.Events)
	}
	if _, _, err := tool.ResolveArgs(ts, json.RawMessage(`{"name":"node","debugger":true}`)); err == nil {
		t.Fatal("a debugger for node resolved")
	}
	if got := ScopeName("go", "1.26.3", false, false); got != "go@1.26" {
		t.Fatalf("scope %s", got)
	}
}

// Names that differ only in case, as a Linux build's terminfo holds, unpack
// on a case-insensitive disk too: the second of the pair is skipped there.
func TestUnpackNamesDifferingInCase(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archive, tarGz(t, entry{name: "t/2621A", body: "a"}, entry{name: "t/2621a", typ: '2', link: "2621A"},
		entry{name: "t/hp", body: "x"}, entry{name: "t/HP", body: "y"}), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := unpack(archive, "tar.gz", out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "t", "2621A")); string(b) != "a" {
		t.Fatalf("2621A holds %q", b)
	}
	// A name given twice exactly is still refused.
	if err := os.WriteFile(archive, tarGz(t, entry{name: "t/x", body: "a"}, entry{name: "t/x", body: "b"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unpack(archive, "tar.gz", filepath.Join(dir, "out2")); err == nil {
		t.Fatal("a name given twice unpacked")
	}
}

// An approved install of the latest stable installs the latest patch, though
// an older patch of the same minor is cached, and records why it was chosen;
// resolving the resolved call again changes nothing.
func TestLatestWithAnOlderPatchInstalled(t *testing.T) {
	s, f := session(t)
	f.goRelease("1.22.7", true, goArchive(t, "1.22.7"), linuxAMD64)
	installedMarker(t, s.Cache, linuxAMD64, "go", "1.22.5")
	tool := Tool{S: s}
	ts, err := tools.NewSession(s.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := tool.ResolveArgs(ts, json.RawMessage(`{"name":"go"}`))
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := tool.ResolveArgs(ts, out)
	if err != nil || string(again) != string(out) {
		t.Fatalf("resolving again: %s, was %s (%v)", again, out, err)
	}
	var a toolArgs
	_ = json.Unmarshal(out, &a)
	if a.Name != "go@1.22" || a.Resolved.Version != "1.22.7" || a.Resolved.Why != "latest stable" {
		t.Fatalf("resolved %s", out)
	}
	res := tool.Run(context.Background(), ts, out)
	if res.IsError || !strings.Contains(res.Content, "Go 1.22.7 is installed") {
		t.Fatalf("run %+v", res)
	}
	var recorded []string
	for _, e := range res.Events {
		if x, ok := e.Payload.(agent.ToolchainAction); ok {
			recorded = append(recorded, x.Version+" "+x.Why)
		}
	}
	if len(recorded) != 1 || recorded[0] != "1.22.7 latest stable" {
		t.Fatalf("recorded %v", recorded)
	}
	// Asked for by its minor, the cached patch is what is planned and run.
	out, _, _ = tool.ResolveArgs(ts, json.RawMessage(`{"name":"go","version":"1.21"}`))
	if res := tool.Run(context.Background(), ts, out); res.IsError || !strings.Contains(res.Content, "1.21.13") {
		t.Fatalf("explicit minor %+v", res)
	}
}

// Resolving a call within a cancelled turn asks the release source nothing.
func TestResolveStopsWithTheTurn(t *testing.T) {
	s, f := session(t)
	tool := Tool{S: s}
	ts, _ := tools.NewSession(s.Workspace)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := tool.ResolveArgsContext(ctx, ts, json.RawMessage(`{"name":"go"}`)); err == nil {
		t.Fatal("resolved in a cancelled turn")
	}
	if len(f.asked) != 0 {
		t.Fatalf("the source was asked %v", f.asked)
	}
}
