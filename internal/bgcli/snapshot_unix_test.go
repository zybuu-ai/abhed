//go:build unix

package bgcli

import (
	"os"
	"path/filepath"
	"testing"
)

// readNoFollow reads a plain file at its name, and refuses a link anywhere
// on the way: in a folder in the middle of the path, or as the file itself,
// even when the link stays inside the repository.
func TestReadNoFollow(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, filepath.Join(root, "real", "dir", "f.txt"), "plain\n")
	write(t, filepath.Join(root, "private", "dir", "f.txt"), "TOPSECRET\n")
	if err := os.Symlink("private", filepath.Join(root, "middle")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../private/dir/f.txt", filepath.Join(root, "real", "dir", "last.txt")); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	data, fi, skip, err := readNoFollow(dir, "real/dir/f.txt")
	if err != nil || skip != "" || string(data) != "plain\n" || !fi.Mode().IsRegular() {
		t.Fatalf("a plain file: %q %v %q %v", data, fi, skip, err)
	}
	for name, want := range map[string]string{"middle/dir/f.txt": "reached through a link", "real/dir/last.txt": "a link"} {
		data, _, skip, err := readNoFollow(dir, name)
		if err != nil || skip != want || data != nil {
			t.Errorf("%s: %q %q %v, want skipped as %q", name, data, skip, err, want)
		}
	}
	if _, _, _, err := readNoFollow(dir, "real/none.txt"); !os.IsNotExist(err) {
		t.Fatalf("a missing file: %v", err)
	}
}
