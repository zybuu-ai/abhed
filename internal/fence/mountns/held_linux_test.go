//go:build linux

package mountns

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// heldDir makes a folder of n empty files under a temp folder, and opens it.
func heldDir(t *testing.T, n int) (string, int, uint64) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "held")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	return dir, fd, st.Dev
}

// A folder read over several batches still counts a file's names across them.
func TestHeldAloneAcrossBatches(t *testing.T) {
	dir, fd, dev := heldDir(t, 3*heldWalkBatch)
	if err := os.Link(filepath.Join(dir, "f0"), filepath.Join(dir, "z-inside")); err != nil {
		t.Fatal(err)
	}
	if err := heldAlone(fd, dev); err != nil {
		t.Fatalf("two names inside the folder: %v", err)
	}
	if err := os.Link(filepath.Join(dir, "f1"), filepath.Join(filepath.Dir(dir), "outside")); err != nil {
		t.Fatal(err)
	}
	if err := heldAlone(fd, dev); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("a name outside the folder: %v", err)
	}
}

// A folder over the cap is refused.
func TestHeldAloneRefusesAHugeFolder(t *testing.T) {
	_, fd, dev := heldDir(t, heldWalkEntries+1)
	if err := heldAlone(fd, dev); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("a folder over the cap: %v", err)
	}
}

// Only a folder another user owns and this one cannot search bars an alias:
// a folder of this user's own could be made searchable again.
func TestBarred(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches any folder")
	}
	slash, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(slash) }()
	own := filepath.Join(t.TempDir(), "own")
	if err := os.MkdirAll(filepath.Join(own, "alias"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(own, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(own, 0o700) })
	if barred(slash, filepath.Join(own, "alias")) {
		t.Error("a folder of the user's own bars an alias")
	}
	if barred(slash, filepath.Dir(own)) {
		t.Error("a reachable path is barred")
	}
	root, err := filepath.EvalSymlinks("/root")
	if err != nil || unix.Access(root, unix.X_OK) == nil {
		t.Skip("/root is searchable here, or missing")
	}
	if !barred(slash, filepath.Join(root, "alias")) {
		t.Error("/root, which this user cannot search, does not bar an alias")
	}
}
