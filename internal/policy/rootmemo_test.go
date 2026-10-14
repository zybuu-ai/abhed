package policy

import (
	"path/filepath"
	"strconv"
	"testing"
)

// Root spellings are kept for at most maxRootMemo roots, as a server sees many workspaces.
func TestRootMemoIsBounded(t *testing.T) {
	dir := t.TempDir()
	for i := range maxRootMemo + 50 {
		rootSpellings(filepath.Join(dir, strconv.Itoa(i)))
	}
	rootMemo.Lock()
	n := len(rootMemo.m)
	rootMemo.Unlock()
	if n > maxRootMemo {
		t.Fatalf("%d roots kept, bound %d", n, maxRootMemo)
	}
}
