package sandbox

import (
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// A HOME moved elsewhere (a CI run given a fresh one) does not open the
// account's own home: it stays unreadable, its state and credentials too.
func TestSeatbeltDeniesTheAccountsHomeWhenHomeIsMoved(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Fatalf("no account home: %v", err)
	}
	real := filepath.Clean(u.HomeDir)
	moved := t.TempDir()
	t.Setenv("HOME", moved)
	if got := homes(); len(got) != 2 || got[0] != filepath.Clean(moved) || got[1] != real {
		t.Fatalf("homes() = %v, want [%s %s]", got, moved, real)
	}
	profile := NewProcess(Policy{Workspace: workspace(t)}).seatbeltProfile()
	for _, want := range []string{
		"(deny file-read* (subpath " + quote(real) + "))",
		"(deny file-read* (subpath " + quote(filepath.Join(real, stateDir)) + "))",
		"(deny file-read* (subpath " + quote(filepath.Join(real, ".ssh")) + "))",
		"(deny file-read* (subpath " + quote(moved) + "))",
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("the profile lacks %s", want)
		}
	}
	// Unmoved, the account's home is named once.
	t.Setenv("HOME", real)
	if got := homes(); len(got) != 1 {
		t.Errorf("homes() = %v with HOME unmoved", got)
	}
}

func quote(s string) string { return `"` + s + `"` }
