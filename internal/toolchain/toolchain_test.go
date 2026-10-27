package toolchain

import (
	"archive/tar"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var linuxAMD64 = Platform{OS: "linux", Arch: "amd64"}

func goArchive(t *testing.T, version string) []byte {
	return tarGz(t,
		entry{name: "go/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "go/bin/go", body: "#!/bin/sh\necho go" + version + "\n", mode: 0o755},
		entry{name: "go/VERSION", body: "go" + version})
}

func TestParseSpecAndVersions(t *testing.T) {
	for in, want := range map[string]string{"go@1.22": "go@1.22", "Python 3.11": "python@3.11", "nodejs": "node",
		"golang@go1.21.3": "go@1.21.3", "node@v20": "node@20", "java 21": "java@21", "ruby@ruby-3.3.0": "ruby@3.3.0", "node@lts/*": "node"} {
		s, err := ParseSpec(in)
		if err != nil || s.String() != want {
			t.Errorf("%q: %v %v", in, s, err)
		}
	}
	for _, bad := range []string{"cobol", "go@../../etc", "python@3.11; rm -rf", "go@1.22.x", "node@$(id)"} {
		if s, err := ParseSpec(bad); err == nil {
			t.Errorf("%q accepted as %v", bad, s)
		}
	}
	if !Matches("1.22.5", "1.22") || Matches("1.220", "1.22") || !Matches("3.11", "3.11") || Matches("3.1", "3.11") {
		t.Fatal("Matches")
	}
	if Major("go", "1.22.5") != "1.22" || Major("node", "20.11.0") != "20" || Major("python", "3.11.4") != "3.11" {
		t.Fatal("Major")
	}
}

// Go: the newest stable matching the request, verified against its sha256,
// unpacked read-only into <root>/<platform>/go/<version>.
func TestResolveAndInstallGo(t *testing.T) {
	f := newFake(t)
	f.goRelease("1.21.13", true, goArchive(t, "1.21.13"), linuxAMD64)
	f.goRelease("1.22.5", true, goArchive(t, "1.22.5"), linuxAMD64)
	f.goRelease("1.23rc1", false, goArchive(t, "1.23rc1"), linuxAMD64)
	src := f.source()
	ctx := context.Background()
	a, err := src.Resolve(ctx, Spec{Lang: "go"}, linuxAMD64)
	if err != nil || a.Version != "1.22.5" {
		t.Fatalf("latest: %+v %v", a, err)
	}
	if a, err := src.Resolve(ctx, Spec{Lang: "go", Version: "1.21"}, linuxAMD64); err != nil || a.Version != "1.21.13" {
		t.Fatalf("1.21: %+v %v", a, err)
	}
	if _, err := src.Resolve(ctx, Spec{Lang: "go", Version: "1.21"}, Platform{OS: "plan9", Arch: "386"}); err == nil {
		t.Fatal("a platform with no build resolved")
	}
	c := testCache(t)
	var seen int64
	in, err := c.Install(ctx, src, a, func(done, total int64) { seen = done })
	if err != nil {
		t.Fatal(err)
	}
	if in.Dir != filepath.Join(c.Root, "linux-amd64", "go", "1.22.5") || seen != a.Size || in.Digest != a.Digest {
		t.Fatalf("installed %+v (seen %d)", in, seen)
	}
	if b, err := os.ReadFile(filepath.Join(in.Dir, "bin", "go")); err != nil || !strings.Contains(string(b), "go1.22.5") {
		t.Fatalf("bin/go: %q %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(in.Dir, "bin", "go"), []byte("x"), 0o755); err == nil {
		t.Fatal("an installed toolchain is writable")
	}
	if err := os.WriteFile(filepath.Join(in.Dir, "planted"), []byte("x"), 0o644); err == nil {
		t.Fatal("an installed toolchain's directory is writable")
	}
	if got := BinDirs(in); len(got) != 1 || got[0] != filepath.Join(in.Dir, "bin") {
		t.Fatalf("bin dirs %v", got)
	}
	// Installed once: a second install downloads nothing.
	n := len(f.asked)
	if _, err := c.Install(ctx, src, a, nil); err != nil || len(f.asked) != n {
		t.Fatalf("reinstall: %v, %d more requests", err, len(f.asked)-n)
	}
	if l := c.List(linuxAMD64, ""); len(l) != 1 || l[0].Version != "1.22.5" {
		t.Fatalf("list %+v", l)
	}
	if b, ok := c.Best(linuxAMD64, Spec{Lang: "go", Version: "1.22"}); !ok || b.Version != "1.22.5" {
		t.Fatal("best")
	}
	if err := c.Remove(linuxAMD64, "go", "1.22.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(in.Dir); !os.IsNotExist(err) {
		t.Fatalf("removed: %v", err)
	}
}

// A download that does not match its published checksum, or is longer than
// published, is thrown away and nothing is installed.
func TestInstallRefusesABadDownload(t *testing.T) {
	f := newFake(t)
	good := goArchive(t, "1.22.5")
	f.goRelease("1.22.5", true, good, linuxAMD64)
	src := f.source()
	ctx := context.Background()
	a, err := src.Resolve(ctx, Spec{Lang: "go"}, linuxAMD64)
	if err != nil {
		t.Fatal(err)
	}
	f.put("/dl/go1.22.5.linux-amd64.tar.gz", goArchive(t, "1.22.6"))
	c := testCache(t)
	if _, err := c.Install(ctx, src, a, nil); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a tampered download: %v", err)
	}
	if _, ok := c.Lookup(linuxAMD64, "go", "1.22.5"); ok {
		t.Fatal("installed anyway")
	}
	if entries, _ := os.ReadDir(filepath.Join(c.Root, ".partial")); len(entries) > 1 {
		t.Fatalf("left behind %v", entries)
	}
	big := append(append([]byte(nil), good...), 0)
	f.put("/dl/go1.22.5.linux-amd64.tar.gz", big)
	if _, err := c.Install(ctx, src, a, nil); err == nil {
		t.Fatal("a download longer than published was installed")
	}
}

// An archive entry or link that leaves the toolchain is refused.
func TestInstallRefusesEscapes(t *testing.T) {
	for name, bad := range map[string][]entry{
		"dotdot":   {{name: "go/../../escape", body: "x"}},
		"absolute": {{name: "/tmp/escape", body: "x"}},
		"link":     {{name: "go/bin/x", typ: tar.TypeSymlink, link: "../../../../etc/passwd"}},
		"abs link": {{name: "go/bin/x", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"device":   {{name: "go/dev", typ: tar.TypeChar}},
	} {
		f := newFake(t)
		f.goRelease("1.22.5", true, tarGz(t, append([]entry{{name: "go/bin/go", body: "x", mode: 0o755}}, bad...)...), linuxAMD64)
		src := f.source()
		a, err := src.Resolve(context.Background(), Spec{Lang: "go"}, linuxAMD64)
		if err != nil {
			t.Fatal(err)
		}
		c := testCache(t)
		root := c.Root
		if _, err := c.Install(context.Background(), src, a, nil); err == nil {
			t.Errorf("%s: installed", name)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(root), "escape")); err == nil {
			t.Errorf("%s: wrote outside", name)
		}
	}
}

// A download URL, or a redirect, off the language's official hosts is refused.
func TestOnlyOfficialHosts(t *testing.T) {
	f := newFake(t)
	f.goRelease("1.22.5", true, goArchive(t, "1.22.5"), linuxAMD64)
	src := f.source()
	a, err := src.Resolve(context.Background(), Spec{Lang: "go"}, linuxAMD64)
	if err != nil {
		t.Fatal(err)
	}
	c := testCache(t)
	off := a
	off.URL = "https://example.com/go.tar.gz"
	if _, err := c.Install(context.Background(), src, off, nil); err == nil || !strings.Contains(err.Error(), "not an official") {
		t.Fatalf("an off-host URL: %v", err)
	}
	f.put("/dl/go1.22.5.linux-amd64.tar.gz", []byte("REDIRECT https://example.com/go.tar.gz"))
	if _, err := c.Install(context.Background(), src, a, nil); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("an off-host redirect: %v", err)
	}
	// The official sources name only https hosts.
	for _, e := range []string{Official.GoDL, Official.NodeDist, Official.PythonAPI, Official.RustDist, Official.AdoptiumAPI,
		Official.MavenRepo, Official.GradleAPI, Official.DotnetIndex, Official.RubyDownload} {
		if !strings.HasPrefix(e, "https://") {
			t.Errorf("%s is not https", e)
		}
	}
}

// Node: the newest LTS by default; the sha256 from the release's SHASUMS256.txt.
func TestResolveNode(t *testing.T) {
	f := newFake(t)
	f.put("/dist/index.json", []byte(`[
	 {"version":"v26.1.0","files":["linux-x64","osx-arm64-tar"],"lts":false},
	 {"version":"v24.9.0","files":["linux-x64","osx-arm64-tar"],"lts":"Krypton"},
	 {"version":"v22.20.0","files":["linux-x64","osx-arm64-tar"],"lts":"Jod"}]`))
	arch := tarGz(t, entry{name: "node-v24.9.0-linux-x64/bin/node", body: "node", mode: 0o755})
	f.put("/dist/v24.9.0/node-v24.9.0-linux-x64.tar.gz", arch)
	f.put("/dist/v24.9.0/SHASUMS256.txt", []byte(sum(arch)+"  node-v24.9.0-linux-x64.tar.gz\n"+strings.Repeat("0", 64)+"  other\n"))
	f.put("/dist/v26.1.0/SHASUMS256.txt", []byte(strings.Repeat("a", 64)+"  node-v26.1.0-darwin-arm64.tar.gz\n"))
	src := f.source()
	a, err := src.Resolve(context.Background(), Spec{Lang: "node"}, linuxAMD64)
	if err != nil || a.Version != "24.9.0" || a.Digest != "sha256:"+sum(arch) {
		t.Fatalf("lts: %+v %v", a, err)
	}
	if a, err := src.Resolve(context.Background(), Spec{Lang: "node", Version: "26"}, Platform{"darwin", "arm64"}); err != nil ||
		!strings.HasSuffix(a.URL, "node-v26.1.0-darwin-arm64.tar.gz") {
		t.Fatalf("26: %+v %v", a, err)
	}
	in, err := testCache(t).Install(context.Background(), src, a0(t, src, "node", linuxAMD64), nil)
	if err != nil || filepath.Base(in.Dir) != "24.9.0" {
		t.Fatalf("install %+v %v", in, err)
	}
	if _, err := os.Stat(filepath.Join(in.Dir, "bin", "node")); err != nil {
		t.Fatal(err)
	}
}

func a0(t *testing.T, src *Source, lang string, p Platform) Artifact {
	t.Helper()
	a, err := src.Resolve(context.Background(), Spec{Lang: lang}, p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Python: python-build-standalone's install_only build, checked against the
// release's SHA256SUMS.
func TestResolvePython(t *testing.T) {
	f := newFake(t)
	arch := tarGz(t, entry{name: "python/bin/python3", body: "py", mode: 0o755})
	names := []string{"cpython-3.12.7+20261021-x86_64-unknown-linux-gnu-install_only.tar.gz",
		"cpython-3.13.1+20261021-x86_64-unknown-linux-gnu-install_only.tar.gz",
		"cpython-3.14.0a1+20261021-x86_64-unknown-linux-gnu-install_only.tar.gz",
		"cpython-3.13.1+20261021-x86_64-unknown-linux-gnu-install_only_stripped.tar.gz"}
	var assets, sums []string
	for _, n := range names {
		u := f.put("/dl/pbs/"+n, arch)
		assets = append(assets, fmt.Sprintf(`{"name":%q,"browser_download_url":%q,"size":%d}`, n, u, len(arch)))
		sums = append(sums, sum(arch)+"  "+n)
	}
	su := f.put("/dl/pbs/SHA256SUMS", []byte(strings.Join(sums, "\n")))
	assets = append(assets, fmt.Sprintf(`{"name":"SHA256SUMS","browser_download_url":%q}`, su))
	f.put("/repos/pbs/releases/latest", []byte(`{"tag_name":"20261021","assets":[`+strings.Join(assets, ",")+`]}`))
	src := f.source()
	a, err := src.Resolve(context.Background(), Spec{Lang: "python"}, linuxAMD64)
	if err != nil || a.Version != "3.13.1" || !strings.Contains(a.URL, "install_only.tar.gz") || a.Digest != "sha256:"+sum(arch) {
		t.Fatalf("latest: %+v %v", a, err)
	}
	if !strings.Contains(a.Source, "not python.org") {
		t.Fatalf("source %q", a.Source)
	}
	if a, err := src.Resolve(context.Background(), Spec{Lang: "python", Version: "3.12"}, linuxAMD64); err != nil || a.Version != "3.12.7" {
		t.Fatalf("3.12: %+v %v", a, err)
	}
	if _, err := testCache(t).Install(context.Background(), src, a, nil); err != nil {
		t.Fatal(err)
	}
}

// Project files pin versions; anything but plain numbers is left out.
func TestPins(t *testing.T) {
	ws := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\n\ngo 1.22\n\ntoolchain go1.22.5\n")
	write(".python-version", "3.11.4\n")
	write(".nvmrc", "v20\n")
	write("rust-toolchain.toml", "[toolchain]\nchannel = \"1.80.0\"\n")
	write(".java-version", "21\n")
	write(".ruby-version", "ruby-3.3.0\n")
	write("global.json", `{"sdk":{"version":"8.0.100"}}`)
	write(".tool-versions", "maven 3.9.9\ngradle 8.10\nnodejs 18\n")
	got := map[string]string{}
	for lang, p := range Pins(ws) {
		got[lang] = p.Spec.Version + " " + p.File
	}
	want := map[string]string{"go": "1.22.5 go.mod", "python": "3.11.4 .python-version", "node": "20 .nvmrc",
		"rust": "1.80.0 rust-toolchain.toml", "java": "21 .java-version", "ruby": "3.3.0 .ruby-version",
		"dotnet": "8.0.100 global.json", "maven": "3.9.9 .tool-versions", "gradle": "8.10 .tool-versions"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("pins\n got %v\nwant %v", got, want)
	}
	ws2 := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws2, ".python-version"), []byte("pypy3.10\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ws2, "rust-toolchain"), []byte("nightly-2024-01-01\n"), 0o644)
	_ = os.Symlink("/etc/passwd", filepath.Join(ws2, ".nvmrc"))
	if p := Pins(ws2); len(p) != 0 {
		t.Fatalf("pins from names, nightlies and links: %v", p)
	}
	s, why := Choose(Spec{Lang: "go"}, Pins(ws))
	if s.Version != "1.22.5" || why != "pinned by go.mod" {
		t.Fatal(s, why)
	}
	s, why = Choose(Spec{Lang: "go", Version: "1.21"}, Pins(ws))
	if s.Version != "1.21" || why != "requested" {
		t.Fatal(s, why)
	}
}

// A command in a subfolder finds the project's pin at the workspace root,
// never a pin file above the root.
func TestPinsStopAtTheWorkspaceRoot(t *testing.T) {
	above := t.TempDir()
	if err := os.WriteFile(filepath.Join(above, ".tool-versions"), []byte("ruby 2.7.8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(above, "ws")
	sub := filepath.Join(root, "pkg", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if p := PinsWithin(sub, root); len(p) != 0 {
		t.Fatalf("a pin above the root was read: %v", p)
	}
	if p := PinsFrom(root); len(p) != 0 {
		t.Fatalf("a pin above the root was read: %v", p)
	}
	if err := os.WriteFile(filepath.Join(root, ".python-version"), []byte("3.12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := PinsWithin(sub, root); p["python"].Spec.Version != "3.12" {
		t.Fatalf("the root's pin from a subfolder: %v", p)
	}
	s := &Session{Workspace: sub, Root: root}
	if _, why := Choose(Spec{Lang: "ruby"}, s.pins(sub)); why != "latest stable" {
		t.Fatalf("ruby %s", why)
	}
}

// The project's virtual environment, the nearest .venv or venv at or above
// the command's folder within the workspace, comes first on PATH.
func TestProjectVenv(t *testing.T) {
	root := t.TempDir()
	mkVenv := func(v string) {
		_ = os.MkdirAll(filepath.Join(v, "bin"), 0o755)
		_ = os.WriteFile(filepath.Join(v, "pyvenv.cfg"), []byte("home = /usr/bin\n"), 0o644)
		_ = os.WriteFile(filepath.Join(v, "bin", "python"), []byte("#!/bin/sh\n"), 0o755)
	}
	sub := filepath.Join(root, "pkg", "x")
	_ = os.MkdirAll(sub, 0o755)
	if ProjectVenv(sub, root) != "" {
		t.Fatal("a venv where there is none")
	}
	// A folder named .venv that is not one is passed by.
	_ = os.MkdirAll(filepath.Join(root, ".venv"), 0o755)
	if ProjectVenv(sub, root) != "" {
		t.Fatal("an empty .venv taken for a venv")
	}
	mkVenv(filepath.Join(root, ".venv"))
	if got := ProjectVenv(sub, root); got != filepath.Join(root, ".venv") {
		t.Fatalf("venv %q", got)
	}
	s := &Session{Workspace: sub, Root: root}
	pre := s.Prelude(sub)
	if !strings.Contains(pre, "export VIRTUAL_ENV="+shellQuote(filepath.Join(root, ".venv"))) || !strings.Contains(pre, "unset PYTHONHOME") {
		t.Fatalf("prelude %q", pre)
	}
	// Never above the workspace root.
	above := t.TempDir()
	mkVenv(filepath.Join(above, "venv"))
	inner := filepath.Join(above, "ws")
	_ = os.MkdirAll(inner, 0o755)
	if ProjectVenv(inner, inner) != "" {
		t.Fatal("a venv above the root was taken")
	}
}
