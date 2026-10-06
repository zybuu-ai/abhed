package store

import (
	"strings"
	"testing"
)

// The title rules every way a title reaches a list shares.
func TestCleanTitle(t *testing.T) {
	for _, raw := range []string{"notes\u202edm", "a\tb", "two\nlines", "zero\u200bwidth", strings.Repeat("x", 121), "bad\xff"} {
		if title, why := CleanTitle(raw); why == "" {
			t.Errorf("CleanTitle(%q) = %q, want refused", raw, title)
		}
	}
	for raw, want := range map[string]string{"  Release notes ": "Release notes", "dev \U0001F468\u200d\U0001F4BB": "dev \U0001F468\u200d\U0001F4BB", "Zebra\u00a0renamed": "Zebra\u00a0renamed"} {
		if title, why := CleanTitle(raw); why != "" || title != want {
			t.Errorf("CleanTitle(%q) = %q %q, want %q", raw, title, why, want)
		}
	}
}
