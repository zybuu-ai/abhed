//go:build !unix

package ui

import (
	"os"
	"time"
)

// probeTerminal cannot wait on a console handle, so it only asks: answers
// that come are recognised by the key reader.
func probeTerminal(_, out *os.File, _ time.Duration) (string, int, []byte) {
	_, _ = out.WriteString(terminalQueries)
	return "", -1, nil
}

const terminalQueries = "\x1b]11;?\x07\x1b[?2026$p\x1b[c"
