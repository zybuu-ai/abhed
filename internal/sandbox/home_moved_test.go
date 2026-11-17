package sandbox

import (
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A HOME moved elsewhere (a CI run given a fresh one) does not open the
// account's own home: it stays unreadable, its state and credentials too.
func TestSeatbeltDeniesTheAccountsHomeWhenHomeIsMoved(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("this uid has no account home in the user database: %v", err)
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

// The account-style home that holds a moved HOME is denied whatever the user
// database says, so a lookup that fails or lies does not open it.
func TestAccountHomeOf(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/runner/.abhed-action.x": "/Users/runner",
		"/Users/runner":                 "/Users/runner",
		"/home/ci/work/h":               "/home/ci",
		"/var/folders/x/T/h":            "/var/folders/x/T/h",
		"/Users":                        "/Users",
		"/Users/Shared/ci/home":         "/Users/Shared/ci/home",
		"/Users/shared/x":               "/Users/shared/x",
	} {
		if got := accountHomeOf(in); got != want {
			t.Errorf("accountHomeOf(%q) = %q, want %q", in, got, want)
		}
	}
	t.Setenv("HOME", "/Users/someone-else/.abhed-action.test")
	if got := homes(); !slices.Contains(got, "/Users/someone-else") {
		t.Errorf("homes() = %v: the home holding a moved HOME is not denied", got)
	}
}
