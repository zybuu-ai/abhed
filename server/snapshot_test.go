package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The snapshot returns before contents are read; a file changed before its
// content was read is still listed, without a diff it cannot honestly give.
func TestSnapshotFillsAfterReturning(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "a\n")
	write("b.txt", "b\n")
	s := takeSnapshot(dir)
	<-s.filled
	if !s.files["a.txt"].hashed || !s.files["a.txt"].kept {
		t.Fatalf("a.txt was not read: %+v", s.files["a.txt"])
	}
	// As if b.txt had changed before the fill reached it.
	s.files["b.txt"].hashed, s.files["b.txt"].kept, s.files["b.txt"].text = false, false, nil
	write("b.txt", "B\n")
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "b.txt"), future, future)
	got, _ := s.changes(nil)
	if len(got) != 1 || got[0].rel != "b.txt" || got[0].status != "modified" || got[0].diffable {
		t.Fatalf("changes: %+v", got)
	}
}

// A Changes poll reads a changed file once; the next poll reuses what it found
// until the file's stat moves again.
func TestSnapshotPollsReuseWhatTheyRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := takeSnapshot(dir)
	<-s.filled
	if err := os.WriteFile(p, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(p, later, later)
	if got, _ := s.changes(nil); len(got) != 1 || string(got[0].after) != "two\n" {
		t.Fatalf("first poll: %+v", got)
	}
	// The cached copy is used: what the poll found stays until the stat moves.
	s.seen["a.txt"].data = []byte("cached\n")
	if got, _ := s.changes(nil); len(got) != 1 || string(got[0].after) != "cached\n" {
		t.Fatalf("second poll read the file again: %+v", got)
	}
	evenLater := later.Add(time.Minute)
	_ = os.Chtimes(p, evenLater, evenLater)
	if got, _ := s.changes(nil); len(got) != 1 || string(got[0].after) != "two\n" {
		t.Fatalf("a moved stat was not read again: %+v", got)
	}
	// Changed back to the snapshot's stat: no longer listed, and forgotten.
	_ = os.WriteFile(p, []byte("one\n"), 0o644)
	_ = os.Chtimes(p, s.files["a.txt"].mod, s.files["a.txt"].mod)
	if got, _ := s.changes(nil); len(got) != 0 || s.seen["a.txt"] != nil {
		t.Fatalf("a restored file: %+v", got)
	}
}
