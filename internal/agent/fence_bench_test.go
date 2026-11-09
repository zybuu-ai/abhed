package agent

import (
	"strings"
	"testing"
)

// goLike is size bytes of Go-like text with '<' in it and no tag.
func goLike(size int, line string) string {
	return strings.Repeat(line, size/len(line)+1)[:size]
}

const (
	// codeLine has a '<' and the word memory: the slow path.
	codeLine = "\tif a < b && len(xs) <= n { return fmt.Errorf(\"bad %d < %d\", a, b) } // memory ok\n"
	// plainLine has a '<' and no memory: what most output is.
	plainLine = "\tif a < b && len(xs) <= n { return fmt.Errorf(\"bad %d < %d\", a, b) } // all ok\n"
)

func benchEscape(b *testing.B, size int, extra string) { benchLines(b, size, codeLine, extra) }

func benchLines(b *testing.B, size int, line, extra string) {
	s := goLike(size, line) + extra
	b.SetBytes(int64(len(s)))
	b.ReportAllocs()
	for b.Loop() {
		_ = escapeFolderFence(s)
	}
}

func BenchmarkEscape1MB(b *testing.B)        { benchEscape(b, 1<<20, "") }
func BenchmarkEscape4MB(b *testing.B)        { benchEscape(b, 4<<20, "") }
func BenchmarkEscape1MBUnicode(b *testing.B) { benchEscape(b, 1<<20, "é") }
func BenchmarkEscape4MBUnicode(b *testing.B) { benchEscape(b, 4<<20, "é") }

func BenchmarkEscape1MBPlain(b *testing.B) { benchLines(b, 1<<20, plainLine, "") }
func BenchmarkEscape4MBPlain(b *testing.B) { benchLines(b, 4<<20, plainLine, "") }
