package server

import "sync/atomic"

// tunable is a package setting a test may change while a goroutine of an
// earlier test, or of the server under test, still reads it: an atomic, so
// the change is never a data race.
type tunable[T any] struct{ p atomic.Pointer[T] }

func newTunable[T any](v T) *tunable[T] {
	t := &tunable[T]{}
	t.p.Store(&v)
	return t
}

// Get is the current value.
func (t *tunable[T]) Get() T { return *t.p.Load() }

// Set replaces the value and returns the one it replaced.
func (t *tunable[T]) Set(v T) T { return *t.p.Swap(&v) }
