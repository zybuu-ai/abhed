package local

import (
	"bytes"
	"context"
	"testing"
)

// The local record answers the same chain API as the database: a synced
// head, and ranges that hold up to the first line that does not.
func TestLocalChainSource(t *testing.T) {
	s := openTest(t, t.TempDir())
	record(t, s, "s-1", "one", "two", "three", "four")
	if err := s.Sync("s-1"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src := s.Chain()
	h, err := src.Head(ctx, "s-1")
	if err != nil || h.Pos != 4 || h.Chain != "local/session" || h.Scope != "s-1" {
		t.Fatalf("head = %+v, %v", h, err)
	}
	r, err := src.VerifyRange(ctx, "s-1", 2, 0)
	first, _, _ := s.HashAt("s-1", 1)
	if err != nil || !r.OK() || r.To != 4 || r.Hash != h.Hash || r.Before != first {
		t.Fatalf("range = %+v, %v", r, err)
	}
	if err := s.Release("s-1"); err != nil {
		t.Fatal(err)
	}
	tamper(t, s, "s-1", func(l [][]byte) [][]byte {
		l[2] = bytes.Replace(l[2], []byte("three"), []byte("THREE"), 1)
		return l
	})
	if r, err := src.VerifyRange(ctx, "s-1", 1, 2); err != nil || !r.OK() {
		t.Fatalf("lines before the edit = %+v, %v", r, err)
	}
	if r, err := src.VerifyRange(ctx, "s-1", 1, 4); err != nil || r.OK() || r.FirstBad != 3 {
		t.Fatalf("a range over the edit = %+v, %v", r, err)
	}
}
