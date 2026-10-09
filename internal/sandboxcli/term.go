package sandboxcli

import (
	"os"

	"golang.org/x/term"
)

// osTerminal is the terminal stdin is on.
type osTerminal struct{ in, out *os.File }

// NewTerminal returns the terminal in and out are on, or nil when either
// is not a terminal.
func NewTerminal(in, out *os.File) Terminal {
	if in == nil || out == nil || !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) { // #nosec G115 -- a file descriptor
		return nil
	}
	return osTerminal{in: in, out: out}
}

func (t osTerminal) MakeRaw() (func(), error) {
	st, err := term.MakeRaw(int(t.in.Fd())) // #nosec G115 -- a file descriptor
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(int(t.in.Fd()), st) }, nil // #nosec G115 -- a file descriptor
}

func (t osTerminal) Size() (uint16, uint16, bool) {
	cols, rows, err := term.GetSize(int(t.out.Fd())) // #nosec G115 -- a file descriptor
	if err != nil || rows <= 0 || cols <= 0 || rows > 4096 || cols > 4096 {
		return 0, 0, false
	}
	return uint16(rows), uint16(cols), true // #nosec G115 -- bounded above
}
