//go:build unix

package ui

import (
	"os"
	"strings"
	"time"
)

// terminalQueries asks for the background colour (OSC 11), whether
// synchronized output is known (DECRQM 2026), and the device attributes,
// which every terminal answers and so ends the wait.
const terminalQueries = "\x1b]11;?\x07\x1b[?2026$p\x1b[c"

// probeTerminal asks the terminal about itself and waits at most wait for
// the answers. It returns the theme its background implies (""), whether it
// knows synchronized output (-1 when it did not say), and anything else that
// arrived meanwhile — keys typed during startup, and answers still in flight,
// which the key reader recognises and applies.
func probeTerminal(in, out *os.File, wait time.Duration) (theme string, sync int, rest []byte) {
	sync = -1
	if _, err := out.WriteString(terminalQueries); err != nil || wait <= 0 {
		return "", sync, nil
	}
	ready := readyFunc(in)
	var buf []byte
	deadline := time.Now().Add(wait)
	tmp := make([]byte, 512)
	for time.Now().Before(deadline) && !daReplied(buf) {
		if !ready(time.Until(deadline)) {
			break
		}
		n, err := in.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	if !daReplied(buf) {
		return "", sync, buf // the key reader takes whatever answers come
	}
	s := string(buf)
	if i := strings.Index(s, "\x1b]11;"); i >= 0 {
		end := strings.IndexByte(s[i:], 0x07)
		if st := strings.Index(s[i:], "\x1b\\"); st >= 0 && (end < 0 || st < end) {
			end = st
		}
		if end > 0 {
			theme = themeFromOSC11(s[i : i+end])
		}
	}
	if i := strings.Index(s, "\x1b[?2026;"); i >= 0 && i+9 < len(s) {
		sync = map[bool]int{true: 1, false: 0}[s[i+8] == '1' || s[i+8] == '2']
	}
	// The answers are consumed here; the key reader gets the rest.
	return theme, sync, []byte(stripReplies(s))
}

func daReplied(buf []byte) bool {
	s := string(buf)
	i := strings.Index(s, "\x1b[?")
	for i >= 0 {
		j := i + 3
		for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == ';') {
			j++
		}
		if j < len(s) && s[j] == 'c' {
			return true
		}
		k := strings.Index(s[i+1:], "\x1b[?")
		if k < 0 {
			return false
		}
		i += 1 + k
	}
	return false
}

// stripReplies removes complete terminal answers from s.
func stripReplies(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == ']' {
			end := strings.IndexByte(s[i:], 0x07)
			if st := strings.Index(s[i:], "\x1b\\"); st >= 0 && (end < 0 || st < end) {
				end = st + 1
			}
			if end > 0 {
				i += end + 1
				continue
			}
		}
		if strings.HasPrefix(s[i:], "\x1b[?") {
			j := i + 3
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == ';' || s[j] == '$') {
				j++
			}
			if j < len(s) && (s[j] == 'c' || s[j] == 'y') {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
