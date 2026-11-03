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

// OfFile counts the names of the open file itself.
func TestOfFileCountsNames(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if n := OfFile(f); n != 1 {
		t.Fatalf("one name: %d", n)
	}
	if err := os.Link(p, filepath.Join(dir, "b")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if n := OfFile(f); n != 2 {
		t.Fatalf("two names: %d", n)
	}
}
