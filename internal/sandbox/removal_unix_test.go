//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// mode is p's permission bits, or fails the test.
func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// outsideTree is a folder outside the sandbox's own, 0555 with a 0500 folder in it.
func outsideTree(t *testing.T) (top, sub string) {
	t.Helper()
	top = t.TempDir()
	sub = filepath.Join(top, "sub")
	if err := os.Mkdir(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(top, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(top, 0o700); _ = os.Chmod(sub, 0o700) })
	return top, sub
}

// A command that swaps the sandbox's own folder for a link to another
// leaves that folder as it was; the link alone is removed.
func TestRemovalNeverFollowsALinkAtItsFolder(t *testing.T) {
	outside, sub := outsideTree(t)
	dir := filepath.Join(t.TempDir(), "abhed-caches-x")
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	makeRemovable(dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, outside); m != 0o555 {
		t.Fatalf("the folder the link named became %v", m)
	}
	if m := mode(t, sub); m != 0o500 {
		t.Fatalf("a folder inside what the link named became %v", m)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("the link was not removed: %v", err)
	}
}

// A link inside is not followed either, and a tree a command made
// unreadable, its top folder included, is still removed.
func TestRemovalMakesItsOwnTreeRemovable(t *testing.T) {
	outside, sub := outsideTree(t)
	dir := filepath.Join(t.TempDir(), "abhed-tmp-x")
	inner := filepath.Join(dir, "mod", "x")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "f"), nil, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "swapped")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{inner, filepath.Dir(inner)} {
		_ = os.Chmod(p, 0o500)
	}
	_ = os.Chmod(dir, 0o000)
	makeRemovable(dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("the tree was not made removable: %v", err)
	}
	if m := mode(t, outside); m != 0o555 {
		t.Fatalf("the folder a link inside named became %v", m)
	}
	if m := mode(t, sub); m != 0o500 {
		t.Fatalf("a folder inside what the link named became %v", m)
	}
}
