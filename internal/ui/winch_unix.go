//go:build unix

package ui

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// resizeSignalled is whether the terminal announces a new size with a signal;
// where it does not, the dock polls the size instead.
const resizeSignalled = true

// watchResize calls f once a burst of resize signals has settled: dragging a
// window's edge sends dozens, and each repaint is a whole screen.
func watchResize(done <-chan struct{}, f func()) {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		var settle <-chan time.Time
		for {
			select {
			case <-done:
				return
			case <-ch:
				settle = time.After(40 * time.Millisecond)
			case <-settle:
				settle = nil
				f()
			}
		}
	}()
}
