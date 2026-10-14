//go:build unix

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// A read-only tree deeper than any bound the walk could hold is still removed.
func TestRemovalHasNoDepthLimit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "abhed-tmp-x")
	deep := filepath.Join(dir, strings.Repeat("d"+string(filepath.Separator), 300))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	for p := deep; p != filepath.Dir(dir); p = filepath.Dir(p) {
		_ = os.Chmod(p, 0o500)
	}
	makeRemovable(dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("a tree 300 folders deep was not made removable: %v", err)
	}
}

// Removal needs only the folders: no file's mode is changed, so a file
// hard-linked to one outside keeps the outside file's mode.
func TestRemovalChangesNoFile(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(outside, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "abhed-tmp-x")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dir, "own")
	if err := os.WriteFile(own, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o500)
	makeRemovable(dir)
	if m := mode(t, own); m != 0o400 {
		t.Fatalf("a file of the tree became %v", m)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, outside); m != 0o400 {
		t.Fatalf("the file hard-linked from outside became %v", m)
	}
}

// Caches inside the private temp folder go with it, without an error, even
// when a command made that folder unreadable.
func TestCachesGoWithAnUnreadableTempFolder(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := &Process{policy: Policy{PrivateTemp: true}}
	if s.ownCaches() == nil || s.tmp == "" || !insideAny(s.caches, []string{s.tmp}) {
		t.Skipf("the caches are not inside the private temp folder here: %q in %q", s.caches, s.tmp)
	}
	if err := os.Chmod(s.tmp, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(s.removeCaches(), s.removeTemp()); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if _, err := os.Lstat(s.tmp); !os.IsNotExist(err) {
		t.Fatalf("the temp folder is still there: %v", err)
	}
}
