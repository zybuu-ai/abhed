package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The README named a count that drifted from the code: it claimed 8 while 9
// were defined and 6 reachable. Every defined reason must be emitted somewhere
// in live code, or carry a comment saying it is reserved.
func TestEveryTerminalReasonIsEmittedOrReserved(t *testing.T) {
	root := filepath.Join("..", "..")
	src, err := os.ReadFile(filepath.Join("event.go"))
	if err != nil {
		t.Fatal(err)
	}
	defined := regexp.MustCompile(`(?m)^\s*(?://[^\n]*\n\s*)*(Term[A-Za-z]+)\s+TerminalReason\s*=`).
		FindAllStringSubmatch(string(src), -1)
	if len(defined) == 0 {
		t.Fatal("no terminal reasons found")
	}

	reserved := map[string]bool{}
	for _, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "Reserved, not emitted") {
			reserved["pending"] = true
		}
		if m := regexp.MustCompile(`(Term[A-Za-z]+)\s+TerminalReason`).FindStringSubmatch(line); m != nil && reserved["pending"] {
			reserved[m[1]] = true
			delete(reserved, "pending")
		}
	}

	for _, d := range defined {
		name := d[1]
		if reserved[name] {
			continue
		}
		if !emittedSomewhere(t, root, name) {
			t.Errorf("%s is defined but never emitted; wire it or mark it reserved", name)
		}
	}
}

func emittedSomewhere(t *testing.T, root, name string) bool {
	t.Helper()
	found := false
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || found || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") ||
			strings.HasSuffix(path, "internal/agent/event.go") ||
			strings.HasSuffix(path, "sdk/record.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), name) {
			found = true
		}
		return nil
	})
	return found
}
