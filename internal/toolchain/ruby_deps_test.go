package toolchain

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// fakeDeb is a .deb whose data.tar.xz holds entries.
func fakeDeb(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var data bytes.Buffer
	xw, err := xz.NewWriter(&data)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(xw)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(e.body))
	}
	_ = tw.Close()
	_ = xw.Close()
	var deb bytes.Buffer
	deb.WriteString("!<arch>\n")
	member := func(name string, body []byte) {
		fmt.Fprintf(&deb, "%-16s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "100644", len(body))
		deb.Write(body)
		if len(body)%2 == 1 {
			deb.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.xz", []byte("x"))
	member("data.tar.xz", data.Bytes())
	return deb.Bytes()
}

// A Linux Ruby bundles libyaml from Debian's pinned package, and its own
// libraries and the bundled one come first on LD_LIBRARY_PATH.
func TestRubyBundlesLibyaml(t *testing.T) {
	f := newFake(t)
	deb := fakeDeb(t,
		entry{name: "./usr/lib/x86_64-linux-gnu/libyaml-0.so.2.0.9", body: "ELF yaml"},
		entry{name: "./usr/lib/x86_64-linux-gnu/libyaml-0.so.2", typ: tar.TypeSymlink, link: "libyaml-0.so.2.0.9"},
		entry{name: "./usr/share/doc/libyaml-0-2/copyright", body: "MIT"})
	old := libyaml
	t.Cleanup(func() { libyaml = old })
	libyaml.sha256 = map[string]string{"amd64": sum(deb)}
	libyaml.size = map[string]int64{"amd64": int64(len(deb))}
	f.put("/debian/pool/main/liby/libyaml/libyaml-0-2_0.2.5-1_amd64.deb", deb)
	src := f.source()
	src.Endpoints.DebianPool = f.srv.URL + "/debian/pool/main/"
	top, tmp := t.TempDir(), t.TempDir()
	got, err := bundleLibyaml(context.Background(), src, top, tmp, Artifact{Lang: "ruby", Platform: linuxAMD64})
	if err != nil || len(got) != 1 || got[0].Digest != "sha256:"+sum(deb) {
		t.Fatalf("%+v %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "lib", "abhed-deps", "libyaml-0.so.2")); string(b) != "ELF yaml" {
		t.Fatalf("libyaml %q", b)
	}
	if _, err := os.Stat(filepath.Join(top, "lib", "abhed-deps", "copyright")); err == nil {
		t.Fatal("a file other than the library was copied")
	}
	// macOS builds bundle nothing.
	if got, err := bundleLibyaml(context.Background(), src, t.TempDir(), tmp, Artifact{Lang: "ruby", Platform: Platform{"darwin", "arm64"}}); err != nil || got != nil {
		t.Fatalf("darwin %+v %v", got, err)
	}
	// A package that is not the pinned one is refused.
	libyaml.sha256 = map[string]string{"amd64": strings.Repeat("0", 64)}
	if _, err := bundleLibyaml(context.Background(), src, t.TempDir(), t.TempDir(), Artifact{Lang: "ruby", Platform: linuxAMD64}); err == nil {
		t.Fatal("a package that does not match its pin was used")
	}
	in := Installed{Lang: "ruby", Version: "3.4.6", Platform: "linux-amd64", Dir: "/c/linux-amd64/ruby/3.4.6"}
	if sp := SearchPaths(in)["LD_LIBRARY_PATH"]; len(sp) != 2 || sp[0] != "/c/linux-amd64/ruby/3.4.6/lib" {
		t.Fatalf("search paths %v", sp)
	}
	if _, ok := SearchPaths(Installed{Lang: "ruby", Version: "3.4.6", Platform: "darwin-arm64"})["LD_LIBRARY_PATH"]; ok {
		t.Fatal("a macOS Ruby sets LD_LIBRARY_PATH")
	}
	// The standard library and RubyGems are found where the build is.
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "lib", "ruby", "3.4.0", "aarch64-linux"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "lib", "ruby", "3.4.0", "aarch64-linux", "rbconfig.rb"), nil, 0o644)
	sp := SearchPaths(Installed{Lang: "ruby", Version: "3.4.6", Platform: "linux-arm64", Dir: dir})
	if rl := sp["RUBYLIB"]; len(rl) != 8 || rl[len(rl)-1] != filepath.Join(dir, "lib", "ruby", "3.4.0", "aarch64-linux") ||
		sp["GEM_PATH"][0] != filepath.Join(dir, "lib", "ruby", "gems", "3.4.0") {
		t.Fatalf("ruby paths %v", sp)
	}
	if !strings.Contains(rubySource(linuxAMD64), "libyaml 0.2.5") || strings.Contains(rubySource(Platform{"darwin", "arm64"}), "libyaml") {
		t.Fatal("the source does not name the bundled library")
	}
}

// A Linux Ruby's libraries are found through its wrapper, for Ruby and what
// it starts only: no LD_LIBRARY_PATH for the session's other commands.
func TestRubyWrapperKeepsLibrariesToRuby(t *testing.T) {
	f := newFake(t)
	deb := fakeDeb(t, entry{name: "./usr/lib/x/libyaml-0.so.2.0.9", body: "y"},
		entry{name: "./usr/lib/x/libyaml-0.so.2", typ: tar.TypeSymlink, link: "libyaml-0.so.2.0.9"})
	old := libyaml
	t.Cleanup(func() { libyaml = old })
	libyaml.sha256 = map[string]string{"amd64": sum(deb)}
	libyaml.size = map[string]int64{"amd64": int64(len(deb))}
	f.put("/debian/pool/main/liby/libyaml/libyaml-0-2_0.2.5-1_amd64.deb", deb)
	src := f.source()
	src.Endpoints.DebianPool = f.srv.URL + "/debian/pool/main/"
	top := t.TempDir()
	if _, err := rubyLinux(context.Background(), src, top, t.TempDir(), Artifact{Lang: "ruby", Platform: linuxAMD64}); err != nil {
		t.Fatal(err)
	}
	in := Installed{Lang: "ruby", Version: "3.4.6", Platform: "linux-amd64", Dir: top}
	if b := BinDirs(in); len(b) != 2 || b[0] != filepath.Join(top, "abhed-bin") {
		t.Fatalf("bin dirs %v", b)
	}
	if _, ok := SearchPaths(in)["LD_LIBRARY_PATH"]; ok {
		t.Fatal("LD_LIBRARY_PATH is set for every command")
	}
	w, _ := os.ReadFile(filepath.Join(top, "abhed-bin", "ruby"))
	if !strings.Contains(string(w), `LD_LIBRARY_PATH="$d/lib:$d/lib/abhed-deps`) || !strings.Contains(string(w), `exec "$d/bin/ruby" "$@"`) {
		t.Fatalf("wrapper %s", w)
	}
	if got, err := rubyLinux(context.Background(), src, t.TempDir(), t.TempDir(), Artifact{Lang: "ruby", Platform: Platform{"darwin", "arm64"}}); err != nil || got != nil {
		t.Fatalf("darwin %v %v", got, err)
	}
}

// Rust is agreed to by its minor series, as Go and Python are.
func TestRustScopeIsItsMinorSeries(t *testing.T) {
	if got := Major("rust", "1.82.0"); got != "1.82" {
		t.Fatalf("rust scope %s", got)
	}
}
