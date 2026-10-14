package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLink(t *testing.T) {
	for target, ok := range map[string]bool{
		"b": true, "sub/../b": false, "./b": false, "b/": false, ".": true, "../x": true, "../../x": false, "/etc": false, "": false,
		".abhed": false, "sub/../.ABHED/config.json": false, "a/.abhed/x": false,
	} {
		if err := Link("a/l", target); (err == nil) != ok {
			t.Errorf("Link(a/l, %q): %v", target, err)
		}
	}
}

// A link into Abhed's state is left out of a pack, and counted.
func TestPackLeavesOutLinksIntoState(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(".abhed/config.json", filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	st, err := Pack(context.Background(), dir, &buf, Limits{}, nil)
	if err != nil || st.Entries != 1 || st.Skipped != 1 {
		t.Fatalf("pack: %+v %v", st, err)
	}
}

// A ".." after another link climbs from where that link leads, so only
// leading ".." parts, no more than the link's depth, are judged on text.
func TestLinkChains(t *testing.T) {
	for _, c := range []struct {
		name, target string
		ok           bool
	}{
		{"d1/d2/L", "..", true},
		{"d1/d2/E", "L/../../secret", false},
		{"d1/d2/d3/L", "../..", true},
		{"d1/d2/d3/L", "../../../..", false},
		{"d1/E", "../d1/d2/d3/L/x", true},
		{"E", "d/L/.abhed/config.json", false},
		{"E", "d/L/../.ABHED", false},
	} {
		if err := Link(c.name, c.target); (err == nil) != c.ok {
			t.Errorf("Link(%s, %q): %v", c.name, c.target, err)
		}
	}
}

func tarOf(t *testing.T, links ...[2]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, l := range links {
		if err := tw.WriteHeader(&tar.Header{Name: l[0], Typeflag: tar.TypeSymlink, Linkname: l[1]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestUnpackRefusesChainedLinks(t *testing.T) {
	for name, links := range map[string][][2]string{
		"dotdot after a link": {{"d1/d2/L", ".."}, {"d1/d2/E", "L/../../secret"}},
		"under a link":        {{"x/y", ".."}, {"x/y/z/L", "../../.."}},
		"state via a link":    {{"d/L", ".."}, {"E", "d/L/.abhed/config.json"}},
	} {
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Unpack(context.Background(), tarOf(t, links...), root, Limits{}, nil); err == nil {
			t.Errorf("%s: unpacked", name)
		}
		_ = root.Close()
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := Unpack(context.Background(), tarOf(t, [2]string{"d1/d2/d3/L", "../.."}), root, Limits{}, nil); err != nil {
		t.Fatalf("a link inside was refused: %v", err)
	}
}

// A pack leaves out a link that would escape once its own links resolve.
func TestPackLeavesOutChainedLinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "d1", "d2"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"d1/d2/L": "..", "d1/d2/E": "L/../../secret"} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	st, err := Pack(context.Background(), dir, &bytes.Buffer{}, Limits{}, nil)
	if err != nil || st.Skipped != 1 {
		t.Fatalf("pack: %+v %v", st, err)
	}
}
