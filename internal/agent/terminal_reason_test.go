package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The README named a count that drifted from the code: it claimed 8 while 9
// were defined and 6 reachable. Every defined reason must be emitted in live
// code, or carry a "Reserved, not emitted" comment.
func TestEveryTerminalReasonIsEmittedOrReserved(t *testing.T) {
	src, err := os.ReadFile("event.go")
	if err != nil {
		t.Fatal(err)
	}

	// A reason is reserved when the comment block above it says so.
	block := regexp.MustCompile(`(?m)((?:^\s*//[^\n]*\n)*)\s*(Term[A-Za-z]+)\s+TerminalReason\s*=`)
	live := liveSource(t)

	for _, m := range block.FindAllStringSubmatch(string(src), -1) {
		comment, name := m[1], m[2]
		if strings.Contains(comment, "Reserved, not emitted") {
			continue
		}
		if !strings.Contains(live, name) {
			t.Errorf("%s is defined but never emitted; wire it or mark it reserved", name)
		}
	}
}

// liveSource is every non-test Go file outside the two that only declare or
// re-export the reasons.
func liveSource(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(filepath.Join("..", ".."), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil //nolint:nilerr // an unreadable file is not this test's concern
		}
		switch {
		case strings.HasSuffix(path, "_test.go"),
			strings.HasSuffix(filepath.Join("internal", "agent", "event.go"), path),
			strings.HasSuffix(path, filepath.Join("internal", "agent", "event.go")),
			strings.HasSuffix(path, filepath.Join("sdk", "record.go")):
			return nil
		}
		if data, readErr := os.ReadFile(path); readErr == nil {
			b.Write(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
