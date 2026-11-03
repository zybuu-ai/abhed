//go:build !windows

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// hashFile hashes a regular file and reads nothing else: a link, a device or
// a pipe put in a file's place is "unreadable", never followed or waited on.
func TestHashFileReadsOnlyARegularFile(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(plain, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello"))
	if got := hashFile(plain); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("plain: %s", got)
	}
	link := filepath.Join(dir, "zero")
	if err := os.Symlink("/dev/zero", link); err != nil {
		t.Fatal(err)
	}
	if got := hashFile(link); got != "unreadable" {
		t.Fatalf("a link to /dev/zero was read: %s", got)
	}
	toFile := filepath.Join(dir, "to-a")
	if err := os.Symlink(plain, toFile); err != nil {
		t.Fatal(err)
	}
	if got := hashFile(toFile); got != "unreadable" {
		t.Fatalf("a link was followed: %s", got)
	}
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no fifo here:", err)
	}
	if got := hashFile(fifo); got != "unreadable" {
		t.Fatalf("a pipe: %s", got)
	}
}

// A sparse file can be any size at no cost; hashing it whole would hold up
// the end of the run for hours. Past the bound it is named by size, time and
// inode, at once, and a change to it is still seen.
func TestHashFileBoundsALargeFile(t *testing.T) {
	old := maxHashBytes
	maxHashBytes = 1 << 20
	t.Cleanup(func() { maxHashBytes = old })
	p := filepath.Join(t.TempDir(), "big_test.go")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 40); err != nil { // 1 TiB, sparse
		t.Skip("no sparse files here:", err)
	}
	_ = f.Close()
	start := time.Now()
	first := hashFile(p)
	if time.Since(start) > 2*time.Second || !strings.HasPrefix(first, "large:") {
		t.Fatalf("got %q after %v", first, time.Since(start))
	}
	if err := os.Chtimes(p, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again := hashFile(p); again == first {
		t.Fatal("a change to a large file was not seen")
	}
}
