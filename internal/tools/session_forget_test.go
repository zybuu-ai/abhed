package tools

import (
	"testing"
	"time"
)

type rewinder struct{ forgot time.Time }

func (r *rewinder) ForgetAfter(t time.Time) { r.forgot = t }

type closer struct{ closed bool }

func (c *closer) Close() error { c.closed = true; return nil }

// A value that can rewind is asked to; any other is closed and forgotten.
func TestForgetScopedAfter(t *testing.T) {
	s, err := NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, c := &rewinder{}, &closer{}
	s.Scoped("r", func() any { return r })
	s.Scoped("c", func() any { return c })
	when := time.Now()
	s.ForgetScopedAfter(when)
	if !r.forgot.Equal(when) || s.Scoped("r", nil) != r {
		t.Fatal("the rewinder was not asked, or was dropped")
	}
	if !c.closed || s.Scoped("c", nil) != nil {
		t.Fatal("a value that cannot rewind was kept")
	}
}
