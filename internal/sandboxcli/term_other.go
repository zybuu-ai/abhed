//go:build !unix

package sandboxcli

// Resized never fires where there is no SIGWINCH; the size is sent once.
func (t osTerminal) Resized() (<-chan struct{}, func()) {
	ch := make(chan struct{})
	return ch, func() { close(ch) }
}
