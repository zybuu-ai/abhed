//go:build !linux

package sandbox

// gitWatch is Linux's: elsewhere a planted commondir is taken out only by the
// sweeps (gitGuard.sweep) and as the next command starts.
type gitWatch struct{}

func (gg *gitGuard) watch([]string) {}

func (gg *gitGuard) stop() {
	gg.mu.Lock()
	gg.closed = true
	gg.mu.Unlock()
}
