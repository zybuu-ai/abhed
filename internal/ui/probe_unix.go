//go:build unix

package ui

import (
	"os"
	"strings"
	"time"
)

// probeBackground asks the terminal for its background colour (OSC 11),
// followed by a device-attributes query every terminal answers, so the wait
// ends as soon as the terminal has said all it will. It returns the theme
// the answer implies ("" for none) and whatever else arrived meanwhile —
// keys typed during startup, which belong to the prompt.
func probeBackground(in, out *os.File) (theme string, typed []byte) {
	ready := readyFunc(in)
	if _, err := out.WriteString("\x1b]11;?\x07\x1b[c"); err != nil {
		return "", nil
	}
	var buf []byte
	deadline := time.Now().Add(probeTimeout)
	tmp := make([]byte, 256)
	for time.Now().Before(deadline) {
		if !ready(time.Until(deadline)) {
			break
		}
		n, err := in.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil || daReplied(buf) {
			break
		}
	}
	s := string(buf)
	if i := strings.Index(s, "\x1b]11;"); i >= 0 {
		end := strings.IndexAny(s[i:], "\x07")
		st := strings.Index(s[i:], "\x1b\\")
		if end < 0 || st >= 0 && st < end {
			end = st
		}
		if end > 0 {
			theme = themeFromOSC11(s[i : i+end])
			s = s[:i] + strings.TrimPrefix(strings.TrimPrefix(s[i+end:], "\x07"), "\x1b\\")
		}
	}
	// Drop the device-attributes answer: ESC [ ? ... c.
	if i := strings.Index(s, "\x1b[?"); i >= 0 {
		if j := strings.IndexByte(s[i:], 'c'); j > 0 {
			s = s[:i] + s[i+j+1:]
		}
	}
	return theme, []byte(s)
}

func daReplied(buf []byte) bool {
	s := string(buf)
	i := strings.Index(s, "\x1b[?")
	return i >= 0 && strings.IndexByte(s[i:], 'c') > 0
}
