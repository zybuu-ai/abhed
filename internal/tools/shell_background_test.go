package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// run_in_background is an argument of bash, kept in the canonical arguments
// policy judges, not dropped as an unknown key.
func TestBashKeepsRunInBackground(t *testing.T) {
	canon, dropped, err := CanonicalArgs(Bash{}, json.RawMessage(`{"command":"make","description":"build","run_in_background":true}`))
	if err != nil || len(dropped) != 0 || !strings.Contains(string(canon), `"run_in_background":true`) {
		t.Fatalf("canon %s, dropped %v, err %v", canon, dropped, err)
	}
}

// The ring keeps the last max bytes; a read reports what was dropped or
// skipped, and the cursor moves to the end either way.
func TestShellRingDropsAndSkips(t *testing.T) {
	r := &shellRing{max: 10}
	for i := 0; i < 5; i++ {
		_, _ = r.Write([]byte("0123456789"))
	}
	text, dropped, skipped, next := r.since(0, 0)
	if text != "0123456789" || dropped != 40 || skipped != 0 || next != 50 {
		t.Fatalf("%q %d %d %d", text, dropped, skipped, next)
	}
	_, _ = r.Write([]byte("abcdef"))
	text, dropped, skipped, next = r.since(next, 4)
	if text != "cdef" || dropped != 0 || skipped != 2 || next != 56 {
		t.Fatalf("%q %d %d %d", text, dropped, skipped, next)
	}
	if text, _, _, _ := r.since(next, 0); text != "" {
		t.Fatalf("nothing new, got %q", text)
	}
}

// A cut never splits a character.
func TestShellRingCutsOnCharacters(t *testing.T) {
	r := &shellRing{max: 100}
	_, _ = r.Write([]byte("aé日本"))
	text, _, skipped, _ := r.since(0, 5)
	if text != "本" || skipped != 6 {
		t.Fatalf("%q skipped %d", text, skipped)
	}
}
