package ui

import (
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/ui/vt"
)

// rig drives a dock the way a terminal does: keys go in through a real pipe,
// so the Esc and paste timing that polls the descriptor works as it does on
// a tty, and every byte the dock writes is applied to an emulated screen.
type rig struct {
	t    *testing.T
	in   *os.File
	term *vt.Terminal
	lr   *LineReader
	out  *meter
}

// meter counts bytes on their way to the emulator.
type meter struct {
	mu    sync.Mutex
	term  *vt.Terminal
	n     atomic.Int64
	since atomic.Int64
	log   strings.Builder
}

func (m *meter) Write(p []byte) (int, error) {
	m.n.Add(int64(len(p)))
	m.mu.Lock()
	m.log.Write(p)
	m.mu.Unlock()
	return m.term.Write(p)
}

func (m *meter) mark()        { m.since.Store(m.n.Load()) }
func (m *meter) bytes() int64 { return m.n.Load() - m.since.Load() }

func newRig(t *testing.T, cols, rows int) *rig {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	term := vt.New(cols, rows)
	m := &meter{term: term}
	lr := NewLineReaderOn(r, m, cols, rows)
	lr.d.kr.ready = readyFunc(r)
	g := &rig{t: t, in: w, term: term, lr: lr, out: m}
	t.Cleanup(func() {
		lr.Close()
		w.Close()
		r.Close()
	})
	return g
}

// keys types s, as one write: what a terminal sends for one key or a paste.
func (g *rig) keys(s string) {
	g.t.Helper()
	if _, err := io.WriteString(g.in, s); err != nil {
		g.t.Fatal(err)
	}
}

// typed types s a key at a time, with a pause between keys as a person makes.
func (g *rig) typed(s string) {
	for _, r := range s {
		g.keys(string(r))
		time.Sleep(2 * time.Millisecond)
	}
}

// line waits for the next submitted line.
func (g *rig) line() (string, error) {
	g.t.Helper()
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := g.lr.ReadLine()
		ch <- res{s, err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(3 * time.Second):
		g.t.Fatalf("no line submitted; screen:\n%s", g.term.Dump())
		return "", nil
	}
}

// settle waits until the dock has drawn everything it has been sent.
func (g *rig) settle() {
	g.t.Helper()
	last := int64(-1)
	for i := 0; i < 200; i++ {
		time.Sleep(5 * time.Millisecond)
		n := g.out.n.Load()
		if n == last {
			// Held once more: the reader may still be applying a key.
			time.Sleep(10 * time.Millisecond)
			if g.out.n.Load() == n {
				return
			}
		}
		last = n
	}
}

// waitScreen waits until the screen satisfies pred.
func (g *rig) waitScreen(what string, pred func(screen string) bool) {
	g.t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if pred(g.term.Text()) {
			return
		}
	}
	g.t.Fatalf("screen never showed %s:\n%s", what, g.term.Dump())
}

func (g *rig) waitText(s string) {
	g.t.Helper()
	g.waitScreen(strings.TrimSpace(s), func(screen string) bool { return strings.Contains(screen, s) })
}

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}
