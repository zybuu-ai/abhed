package local

import "testing"

// SealedSince reads lines as written, with their links, for a session held
// here or by nobody; an id the record does not hold is empty.
func TestSealedSinceCarriesTheChain(t *testing.T) {
	s := openTest(t, t.TempDir())
	record(t, s, "s-1", "one", "two", "three")
	lines, err := s.SealedSince("s-1", 1)
	if err != nil || len(lines) != 2 {
		t.Fatalf("lines after seq 1: %d %v", len(lines), err)
	}
	all, _ := s.SealedSince("s-1", 0)
	if all[0].Prev != Genesis || lines[0].Prev != all[0].Hash || lines[1].Prev != lines[0].Hash || !isHash(lines[1].Hash) {
		t.Fatalf("links: %+v", all)
	}
	if lines[0].Seq != 2 || lines[0].SessionID != "s-1" {
		t.Fatalf("event: %+v", lines[0].Event)
	}
	if got, err := s.SealedSince("../etc", 0); got != nil || err != nil {
		t.Fatalf("a bad id: %v %v", got, err)
	}
}
