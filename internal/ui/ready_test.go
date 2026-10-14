package ui

import (
	"io"
	"testing"
	"time"
)

// A key typed after the dock is drawn but before the session has set its
// hotkeys waits for Ready, and then does what the hotkey says: on a slow
// machine it was taken by the default before the hotkey was set.
func TestKeysWaitForTheSessionToBeReady(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	d := newDock(r, io.Discard, Style{})
	d.size = func() (int, int) { return 60, 20 }
	l := &LineReader{d: d, raw: true, done: make(chan struct{})}
	l.start()
	defer close(l.done)
	go func() { _, _ = w.Write([]byte{keyCtrlT}) }()
	time.Sleep(100 * time.Millisecond) // the key has long arrived
	fired := make(chan struct{})
	l.Hotkey("ctrl+t", func() { close(fired) })
	l.Ready()
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("a key typed before the session was ready did not reach its hotkey")
	}
}
