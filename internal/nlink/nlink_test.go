package nlink

import (
	"os"
	"path/filepath"
	"testing"
)

// A second name for a file is counted, on every platform Linked supports.
func TestLinkedCountsASecondName(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := Linked(a); err != nil || n != 0 {
		t.Fatalf("one name: %d %v", n, err)
	}
	if err := os.Link(a, filepath.Join(dir, "b")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if n, err := Linked(a); err != nil || n != 2 {
		t.Fatalf("two names: %d %v", n, err)
	}
}
