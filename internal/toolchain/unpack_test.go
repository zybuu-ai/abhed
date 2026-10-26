package toolchain

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
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
