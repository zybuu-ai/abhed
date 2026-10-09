//go:build unix

package sandboxcli

import (
	"os"
	"os/signal"
	"syscall"
)

func (t osTerminal) Resized() (<-chan struct{}, func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case <-sig:
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out, func() { signal.Stop(sig); close(done) }
}
