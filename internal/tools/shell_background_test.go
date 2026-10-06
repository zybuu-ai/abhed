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
	text, dropped, skipped, next, _ := r.since(0, 0, true)
	if text != "0123456789" || dropped != 40 || skipped != 0 || next != 50 {
		t.Fatalf("%q %d %d %d", text, dropped, skipped, next)
	}
	_, _ = r.Write([]byte("abcdef"))
	text, dropped, skipped, next, _ = r.since(next, 4, true)
	if text != "cdef" || dropped != 0 || skipped != 2 || next != 56 {
		t.Fatalf("%q %d %d %d", text, dropped, skipped, next)
	}
	if text, _, _, _, _ := r.since(next, 0, true); text != "" {
		t.Fatalf("nothing new, got %q", text)
	}
}

// A cut never splits a character.
func TestShellRingCutsOnCharacters(t *testing.T) {
	r := &shellRing{max: 100}
	_, _ = r.Write([]byte("aé日本"))
	text, _, skipped, _, _ := r.since(0, 5, true)
	if text != "本" || skipped != 6 {
		t.Fatalf("%q skipped %d", text, skipped)
	}
}

// A read that ends inside a character leaves it for the next read, which
// then starts on it: nothing is skipped and the output is whole.
func TestShellRingLeavesAPartCharacter(t *testing.T) {
	r := &shellRing{max: 100}
	_, _ = r.Write([]byte("ok \xe6\x97"))
	text, dropped, skipped, next, _ := r.since(0, 0, true)
	if text != "ok " || dropped != 0 || skipped != 0 || next != 3 {
		t.Fatalf("first read %q %d %d %d", text, dropped, skipped, next)
	}
	_, _ = r.Write([]byte("\xa5 more output\n"))
	text, dropped, skipped, next, _ = r.since(next, 0, true)
	if text != "日 more output\n" || dropped != 0 || skipped != 0 || next != r.total {
		t.Fatalf("second read %q %d %d %d", text, dropped, skipped, next)
	}
	// Once the command has ended, a part character is shown as it is.
	_, _ = r.Write([]byte("end \xe6"))
	if text, _, skipped, _, _ := r.since(next, 0, false); text != "end �" || skipped != 0 {
		t.Fatalf("final read %q skipped %d", text, skipped)
	}
	// A read from the cursor skips nothing, even over stray bytes.
	s := &shellRing{max: 100}
	_, _ = s.Write([]byte("\x97\x97ab"))
	if text, _, skipped, _, _ := s.since(0, 0, true); text != "�ab" || skipped != 0 {
		t.Fatalf("stray bytes %q skipped %d", text, skipped)
	}
}
