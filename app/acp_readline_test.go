package app

import (
	"bufio"
	"errors"
	"io"
	"runtime"
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

// A head nested as deep as a line can hold is walked in bounded memory: only
// its first bytes are read, and only so deep.
func TestRequestIDBoundsItsWork(t *testing.T) {
	head := []byte(`{"p":` + strings.Repeat("[", 16<<20))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if id := requestID(head); id != nil {
		t.Fatalf("id %s", id)
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4<<20 {
		t.Fatalf("walking the head allocated %d MiB", grew>>20)
	}
}

// readLine keeps at most headMax bytes of a line too long to use.
func TestReadLineKeepsAShortHead(t *testing.T) {
	rd := bufio.NewReaderSize(strings.NewReader(strings.Repeat("z", 3<<20)+"\n"), 64<<10)
	_, head, tooLong, _ := readLine(rd, 1<<20)
	if !tooLong || len(head) != headMax {
		t.Fatalf("tooLong %v, head %d bytes", tooLong, len(head))
	}
}

// The limit is the line's own bytes, its \r\n aside, ended by a newline or
// by the input.
func TestReadLineLimitIsExact(t *testing.T) {
	for _, tc := range []struct {
		in   string
		long bool
	}{
		{strings.Repeat("a", 10) + "\r\n", false},
		{strings.Repeat("a", 10) + "\n", false},
		{strings.Repeat("a", 10), false},
		{strings.Repeat("a", 11) + "\r\n", true},
		{strings.Repeat("a", 11), true},
	} {
		rd := bufio.NewReaderSize(iotest.OneByteReader(strings.NewReader(tc.in)), 16)
		line, _, tooLong, _ := readLine(rd, 10)
		if tooLong != tc.long || (!tc.long && string(line) != strings.Repeat("a", 10)) {
			t.Errorf("%q: tooLong %v, line %q", tc.in, tooLong, line)
		}
	}
}
