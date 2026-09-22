package server

import "testing"

func TestPlainTextStripsTerminalSequences(t *testing.T) {
	in := "\x1b[?1049h\x1b[>4;2m\"main.go\" 12L, 159B\x1bPzz\x1b\\\x1b]0;title\x07\r\n\x1b[31mred\x1b[0m\x1b(B\x1b[H\x1b[2Jline\x1b[K\n"
	if got := plainText([]byte(in)); got != "\"main.go\" 12L, 159B\nredline\n" {
		t.Fatalf("%q", got)
	}
}
