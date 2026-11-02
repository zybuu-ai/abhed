package toolchain

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// zipOf packs entries into a zip; a link entry is a symbolic link.
func zipOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Store}
		body := e.body
		if e.link != "" {
			h.SetMode(fs.ModeSymlink | 0o777)
			body = e.link
		} else {
			h.SetMode(0o644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	return buf.Bytes()
}

// No chain of links, in any order, a hard link through a link, or a zip
// link chain carries an entry out of the folder it unpacks into.
func TestUnpackRefusesLinkChains(t *testing.T) {
	sym := func(name, link string) entry { return entry{name: name, typ: tar.TypeSymlink, link: link} }
	cases := []struct {
		name    string
		format  string
		entries []entry
	}{
		{"chained dot links", "tar.gz", []entry{sym("t/a", "."), sym("t/a/b", ".."), sym("t/a/b/c", ".."), {name: "t/a/b/c/ESCAPED.txt", body: "x"}}},
		{"a file through a link", "tar.gz", []entry{sym("t/up", ".."), {name: "t/up/ESCAPED.txt", body: "x"}}},
		{"a folder through a link", "tar.gz", []entry{sym("t/up", ".."), {name: "t/up/d/", typ: tar.TypeDir}, {name: "t/up/d/ESCAPED.txt", body: "x"}}},
		{"a hard link through a link", "tar.gz", []entry{{name: "secret", body: "s"}, sym("t/l", ".."), {name: "t/h", typ: tar.TypeLink, link: "t/l/secret"}}},
		{"a hard link to a link", "tar.gz", []entry{sym("t/l", "../.."), {name: "t/h", typ: tar.TypeLink, link: "t/l"}}},
		{"a chain made in the other order", "tar.gz", []entry{sym("t/x", "a/.."), sym("t/a", "..")}},
		{"a dangling link through a dot link", "tar.gz", []entry{sym("go/s", "."), sym("go/x", "s/s/s/../../../../ESCAPED.txt")}},
		{"a target through another link", "tar.gz", []entry{{name: "go/d/f", body: "x"}, sym("go/s", "d"), sym("go/x", "s/f")}},
		{"a dangling link", "tar.gz", []entry{sym("go/x", "missing")}},
		{"a zip link chain", "zip", []entry{{name: "t/a", link: "."}, {name: "t/a/b", link: ".."}, {name: "t/a/b/c", link: ".."}, {name: "t/a/b/c/ESCAPED.txt", body: "x"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			out := filepath.Join(base, "a", "b", "c", "out")
			if err := os.MkdirAll(out, 0o755); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(base, "archive")
			body := zipOf(t, c.entries...)
			if c.format == "tar.gz" {
				body = tarGz(t, c.entries...)
			}
			if err := os.WriteFile(archive, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := unpack(archive, c.format, out); err == nil {
				t.Fatal("unpacked")
			}
			_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if err == nil && d.Name() == "ESCAPED.txt" {
					t.Errorf("an entry escaped to %s", p)
				}
				return nil
			})
		})
	}
	// Links that stay inside, as real toolchains have, still unpack.
	base := t.TempDir()
	archive := filepath.Join(base, "ok.tar.gz")
	_ = os.WriteFile(archive, tarGz(t, entry{name: "n/lib/npm/cli.js", body: "x"}, sym("n/bin/npm", "../lib/npm/cli.js"),
		entry{name: "n/lib/libpy.so.1", body: "y"}, sym("n/lib/libpy.so", "libpy.so.1")), 0o600)
	if err := unpack(archive, "tar.gz", filepath.Join(base, "out")); err != nil {
		t.Fatal(err)
	}
}

// An archive with a dangling link at the marker's name, escaping only through
// a "." link, is refused, and nothing is written outside the toolchain.
func TestMarkerIsNeverWrittenThroughALink(t *testing.T) {
	s, f := session(t)
	evil := tarGz(t,
		entry{name: "go/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "go/bin/go", body: "#!/bin/sh\necho go\n", mode: 0o755},
		entry{name: "go/s", typ: tar.TypeSymlink, link: "."},
		entry{name: "go/" + markerFile, typ: tar.TypeSymlink, link: "s/s/s/../../../../ESCAPED"})
	f.goRelease("1.22.9", true, evil, linuxAMD64)
	tool := Tool{S: s}
	ts, _ := tools.NewSession(s.Workspace)
	out, _, err := tool.ResolveArgs(ts, json.RawMessage(`{"name":"go@1.22"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := tool.Run(context.Background(), ts, out); !r.IsError {
		t.Fatalf("installed: %s", r.Content)
	}
	_ = filepath.WalkDir(filepath.Dir(s.Cache.Root), func(p string, d os.DirEntry, err error) error {
		if d != nil && d.Name() == "ESCAPED" {
			t.Errorf("written outside the toolchain: %s", p)
		}
		return nil
	})
	// A plain file at the marker's name refuses too, rather than being overwritten.
	s2, f2 := session(t)
	f2.goRelease("1.22.9", true, tarGz(t, entry{name: "go/bin/go", body: "x", mode: 0o755}, entry{name: "go/" + markerFile, body: `{"lang":"go"}`}), linuxAMD64)
	tool2 := Tool{S: s2}
	ts2, _ := tools.NewSession(s2.Workspace)
	out2, _, _ := tool2.ResolveArgs(ts2, json.RawMessage(`{"name":"go@1.22"}`))
	if r := tool2.Run(context.Background(), ts2, out2); !r.IsError {
		t.Fatalf("an archive's own marker was kept: %s", r.Content)
	}
}

// A link to a sibling version in the cache is refused: links are checked
// against the toolchain as it will stand.
func TestLinksAreCheckedAgainstTheToolchain(t *testing.T) {
	s, f := session(t)
	f.goRelease("1.22.9", true, tarGz(t, entry{name: "go/bin/go", typ: tar.TypeSymlink, link: "../../1.22.5/bin/go"}), linuxAMD64)
	installedMarker(t, s.Cache, linuxAMD64, "go", "1.22.5")
	if err := os.WriteFile(filepath.Join(s.Cache.Dir(linuxAMD64, "go", "1.22.5"), "bin", "go"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := s.Plan(context.Background(), Spec{Lang: "go", Version: "1.22.9"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Install(context.Background(), plan, "user", "cli", "cli", nil); err == nil {
		t.Fatal("a link to another version was installed")
	}
}

// On a case-insensitive disk a link and a file whose names differ only in
// case collide; the file wins, so no link is left pointing at itself.
func TestUnpackFileWinsOverALinkDifferingInCase(t *testing.T) {
	base := t.TempDir()
	archive := filepath.Join(base, "a.tar.gz")
	_ = os.WriteFile(archive, tarGz(t, entry{name: "t/N/NCR", typ: tar.TypeSymlink, link: "../n/ncr"}, entry{name: "t/n/ncr", body: "real"}), 0o600)
	out := filepath.Join(base, "out")
	if err := unpack(archive, "tar.gz", out); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "t", "n", "ncr")); err != nil || string(b) != "real" {
		t.Fatalf("%q %v", b, err)
	}
}
