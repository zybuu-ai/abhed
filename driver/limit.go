package driver

import (
	"errors"
	"io"
	"sync"
)

// ErrOutputLimit is a stream that passed its total output budget. The
// protocol bounds each frame; this bounds the whole call.
var ErrOutputLimit = errors.New("output limit reached")

// OutputLimit is one byte budget shared by every writer it wraps, so stdout
// and stderr together cannot pass it. The write that crosses the limit
// keeps what fits, fails with ErrOutputLimit, and calls OnExceed once.
type OutputLimit struct {
	mu       sync.Mutex
	left     int64
	exceeded bool
	// OnExceed, when set, runs once when the budget is spent, such as the
	// cancel of the call producing the output.
	OnExceed func()
}

// NewOutputLimit returns a budget of n bytes.
func NewOutputLimit(n int64) *OutputLimit { return &OutputLimit{left: n} }

// Exceeded reports whether a write was refused.
func (l *OutputLimit) Exceeded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exceeded
}

// Wrap returns w drawing on the budget; nil stays nil.
func (l *OutputLimit) Wrap(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	return limitedWriter{l, w}
}

type limitedWriter struct {
	l *OutputLimit
	w io.Writer
}

func (lw limitedWriter) Write(p []byte) (int, error) {
	l := lw.l
	l.mu.Lock()
	if l.exceeded {
		l.mu.Unlock()
		return 0, ErrOutputLimit
	}
	n := int64(len(p))
	over := n > l.left
	if over {
		n = l.left
		l.exceeded = true
	}
	l.left -= n
	exceed := l.OnExceed
	l.mu.Unlock()
	written, err := lw.w.Write(p[:n])
	if over {
		if exceed != nil {
			exceed()
		}
		return written, ErrOutputLimit
	}
	return written, err
}
