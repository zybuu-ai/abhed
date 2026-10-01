package tools

import "testing"

type closeCount struct{ n *int }

func (c closeCount) Close() error { *c.n++; return nil }

// What a tool keeps for a session stays with that session: another session
// never sees it, while a fork of the same conversation does.
func TestScopedStaysWithItsSession(t *testing.T) {
	a, err := NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	type key struct{}
	a.Scoped(key{}, func() any { return "a's login" })
	if v := b.Scoped(key{}, nil); v != nil {
		t.Fatalf("another session sees %v", v)
	}
	if v := a.Fork().Scoped(key{}, nil); v != "a's login" {
		t.Fatalf("a fork of the session lost it: %v", v)
	}
	child, _ := NewSession(t.TempDir())
	child.InheritScoped(a)
	if v := child.Scoped(key{}, nil); v != "a's login" {
		t.Fatalf("a subagent of the session lost it: %v", v)
	}
	var nilSess *Session
	if v := nilSess.Scoped(key{}, func() any { return "x" }); v != nil {
		t.Fatalf("a nil session kept %v", v)
	}
}

func TestCloseScopedClosesAndForgets(t *testing.T) {
	s, _ := NewSession(t.TempDir())
	n := 0
	type key struct{}
	s.Scoped(key{}, func() any { return closeCount{&n} })
	s.CloseScoped()
	if n != 1 {
		t.Fatalf("closed %d times, want 1", n)
	}
	if v := s.Scoped(key{}, nil); v != nil {
		t.Fatalf("still kept after close: %v", v)
	}
	// A call still in flight when the session went keeps nothing either.
	if v := s.Scoped(key{}, func() any { return closeCount{&n} }); v != nil {
		t.Fatalf("kept after close: %v", v)
	}
}

// A reset closes what was kept and leaves the session open for the next
// conversation, unlike CloseScoped, which is for a session that has ended.
func TestResetScopedClosesAndStaysOpen(t *testing.T) {
	s, _ := NewSession(t.TempDir())
	n := 0
	type key struct{}
	s.Scoped(key{}, func() any { return closeCount{&n} })
	s.ResetScoped()
	if n != 1 || s.Scoped(key{}, nil) != nil {
		t.Fatalf("closed %d times, still kept %v", n, s.Scoped(key{}, nil))
	}
	if v := s.Scoped(key{}, func() any { return "next" }); v != "next" {
		t.Fatalf("the session keeps nothing after a reset: %v", v)
	}
}
