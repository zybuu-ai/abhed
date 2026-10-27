package app

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// readLine takes lines read in pieces, a last line with no newline, and a
// line over the limit (also at the end of input) without losing the next.
func TestReadLine(t *testing.T) {
	read := func(in string, max int) (lines []string, long []string) {
		rd := bufio.NewReaderSize(iotest.OneByteReader(strings.NewReader(in)), 16)
		for {
			line, head, tooLong, err := readLine(rd, max)
			if tooLong {
				long = append(long, string(head))
				continue
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, string(line))
		}
	}
	lines, long := read("first line\r\nsecond\n\nlast without newline", 64)
	if strings.Join(lines, "|") != "first line|second||last without newline" || len(long) != 0 {
		t.Fatalf("%q %q", lines, long)
	}
	lines, long = read(strings.Repeat("x", 100)+"\nafter\n"+strings.Repeat("y", 100), 50)
	if strings.Join(lines, "|") != "after" || len(long) != 2 || !strings.HasPrefix(long[0], "xxx") || !strings.HasPrefix(long[1], "yyy") {
		t.Fatalf("%q %q", lines, long)
	}
}

// The id of a message too long to parse is its top-level one, even when a
// nested "id" comes first.
func TestRequestIDIsTheTopLevelOne(t *testing.T) {
	for in, want := range map[string]string{
		`{"jsonrpc":"2.0","params":{"id":"inner","x":[{"id":9}]},"id":"outer","method":"session/prompt","pa`: `"outer"`,
		`{"id":42,"method":"m","params":{"text":"xxxx`:                                                       `42`,
		`{"method":"m","params":{"id":"inner","text":"xxxx`:                                                  ``,
	} {
		if got := string(requestID([]byte(in))); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}
