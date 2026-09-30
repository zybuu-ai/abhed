package local

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

// cutLines keeps the first keep lines of a file and adds extra, as the
// review's probes cut a record.
func cutLines(t *testing.T, path string, keep int, extra string) {
	t.Helper()
	data, _ := os.ReadFile(path)
	ls := bytes.SplitAfter(data, []byte("\n"))
	out := bytes.Join(ls[:keep], nil)
	out = append(out, extra...)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustFail(t *testing.T, s *Store, id, why string) {
	t.Helper()
	rep, err := s.Verify(id)
	if err != nil || rep.OK {
		t.Fatalf("%s: verify passed: %+v %v", why, rep, err)
	}
}

// Whole lines cut from a session nobody ended: opening it for writing
// refuses, and verify still fails afterwards. Nothing rewrites the head.
func TestTruncateThenOpenIsNotLaundered(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two", "three", "four", "five")
	_ = s.Close()
	s2 := openTest(t, dir)
	cutLines(t, s2.Path("s-1"), 3, "")
	head, _ := os.ReadFile(s2.headPath("s-1"))
	mustFail(t, s2, "s-1", "cut")
	if err := s2.Acquire("s-1"); !errors.Is(err, ErrUnverified) {
		t.Fatalf("a cut record was opened for writing: %v", err)
	}
	if _, err := s2.ClaimResume(t.Context(), "s-1"); !errors.Is(err, ErrUnverified) {
		t.Fatalf("a cut record was claimed: %v", err)
	}
	if evs, err := s2.Events("s-1"); err != nil || len(evs) != 3 {
		t.Fatalf("it can still be read: %d %v", len(evs), err)
	}
	mustFail(t, s2, "s-1", "after open")
	if now, _ := os.ReadFile(s2.headPath("s-1")); !bytes.Equal(now, head) {
		t.Fatal("the head was rewritten")
	}
}

// Lines cut with a fragment left behind: a read, as record show does,
// changes nothing, and the fragment is not taken for a crash.
func TestTornCutViaReadPathIsNotLaundered(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two", "three", "four", "five")
	_ = s.Close()
	s2 := openTest(t, dir)
	cutLines(t, s2.Path("s-1"), 2, `{"seq":3,"id":"x`)
	before, _ := os.ReadFile(s2.Path("s-1"))
	if _, err := s2.Events("s-1"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(s2.Path("s-1")); !bytes.Equal(before, after) {
		t.Fatal("a read changed the record")
	}
	mustFail(t, s2, "s-1", "after read")
	if err := s2.Acquire("s-1"); !errors.Is(err, ErrUnverified) {
		t.Fatalf("a cut counted line was repaired as a crash: %v", err)
	}
	mustFail(t, s2, "s-1", "after a refused open")
}

// A counted line that lost only its newline is not torn: it is kept, and a
// line that is whole and chained is completed rather than cut.
func TestMissingNewlineOnACountedLine(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir)
	record(t, s, "s-1", "one", "two")
	_ = s.Close()
	s2 := openTest(t, dir)
	data, _ := os.ReadFile(s2.Path("s-1"))
	_ = os.WriteFile(s2.Path("s-1"), bytes.TrimSuffix(data, []byte("\n")), 0o600)
	if err := s2.Acquire("s-1"); err != nil {
		t.Fatalf("a whole last line without its newline: %v", err)
	}
	evs, _ := s2.Events("s-1")
	if len(evs) != 3 || !strings.Contains(string(evs[2].Payload), "newline was completed") {
		t.Fatalf("events: %d %s", len(evs), evs[len(evs)-1].Payload)
	}
	mustVerify(t, s2, "s-1")
}
