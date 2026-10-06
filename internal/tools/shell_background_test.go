package tools

import (
	"encoding/json"
	"math/rand"
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

// The last line is redacted before it is clipped, and a tail cut from longer
// output drops where a part of a cut secret could be, so neither a clip nor
// the cut shows a part of a stored value.
func TestShellLastLineRedactsBeforeClipping(t *testing.T) {
	const standIn = "lv-standin-5c8e2a7d1f"
	redact := func(s string) string { return strings.ReplaceAll(s, standIn, "[secret:STAND_IN]") }
	leaks := func(s string) bool {
		s = strings.ReplaceAll(s, "[secret:STAND_IN]", "")
		for i := 0; i+3 <= len(standIn); i++ {
			if strings.Contains(s, standIn[i:i+3]) {
				return true
			}
		}
		return false
	}
	lineOf := func(out string) *ShellProc {
		p := &ShellProc{out: &shellRing{max: 1 << 20}}
		_, _ = p.out.Write([]byte(out))
		return p
	}
	// A secret across the clip.
	p := lineOf("first\n" + strings.Repeat(".", 190) + standIn + strings.Repeat(".", 50) + "\n")
	if got := p.LastLine(200, redact, len(standIn)); leaks(got) || !strings.Contains(got, "[secret:") {
		t.Fatalf("across the clip: %q", got)
	}
	if !leaks(redact(p.LastLine(200, nil, 0))) {
		t.Fatal("the clip does not cut the value; the case tests nothing")
	}
	// A secret across the start of the tail the last line is read from.
	for into := 1; into < len(standIn); into++ {
		p := lineOf(strings.Repeat(".", 5000) + standIn + strings.Repeat(".", 4096-len(standIn)+into))
		if got := p.LastLine(200, redact, len(standIn)); leaks(got) {
			t.Fatalf("%d bytes into the value: %q", into, got)
		}
	}
}

// A value that starts inside the bytes a cut tail drops is not split by the
// drop: its end is never shown on its own, for any start in that drop.
func TestShellLastLineDropKeepsWholeValues(t *testing.T) {
	const standIn = "lv-standin-5c8e2a7d1f"
	span := len(standIn)
	redact := func(s string) string { return strings.ReplaceAll(s, standIn, "[secret:STAND_IN]") }
	for at := 1; at <= span-2; at++ {
		out := strings.Repeat(".", 5000) + strings.Repeat("-", at) + standIn + strings.Repeat(".", 4096-at-span)
		p := &ShellProc{out: &shellRing{max: 1 << 20}}
		_, _ = p.out.Write([]byte(out))
		got := p.LastLine(200, redact, span)
		if shown := strings.ReplaceAll(got, "[secret:STAND_IN]", ""); strings.ContainsAny(shown, "abcdefilnstv0123456789") {
			t.Fatalf("value %d bytes into the tail: %q", at, got)
		}
		// A fixed drop of span-1 bytes would show its end.
		tail := out[len(out)-4096:]
		if at >= 2 && !strings.Contains(redact(tail[span-1:]), standIn[span-1-at:span-1-at+2]) {
			t.Fatalf("value %d bytes into the tail: the case tests nothing", at)
		}
	}
}

// Over random output, the last line never shows a part of a stored value
// longer than a few bytes that redacting all the output would not show.
func TestShellLastLineNeverShowsAPart(t *testing.T) {
	const standIn = "fz-standin-93ab17c4e0"
	redact := func(s string) string { return strings.ReplaceAll(s, standIn, "[secret:STAND_IN]") }
	rng := rand.New(rand.NewSource(7))
	const filler = "....----  =="
	for i := range 3000 {
		var sb strings.Builder
		for sb.Len() < 4096+rng.Intn(3000) {
			// Newlines are rare, so the cut tail's start is often on the last line.
			switch c := rng.Intn(60); {
			case c < 8:
				sb.WriteString(standIn)
			case c < 10:
				sb.WriteString(standIn[:rng.Intn(len(standIn))])
			case c == 10:
				sb.WriteByte('\n')
			default:
				for range rng.Intn(40) {
					sb.WriteByte(filler[rng.Intn(len(filler))])
				}
			}
		}
		out := sb.String()
		p := &ShellProc{out: &shellRing{max: 1 << 20}}
		_, _ = p.out.Write([]byte(out))
		got := p.LastLine(1+rng.Intn(5000), redact, len(standIn))
		whole := redact(out)
		for k := 4; k <= len(standIn); k++ {
			for j := 0; j+k <= len(standIn); j++ {
				if part := standIn[j : j+k]; strings.Contains(got, part) && !strings.Contains(whole, part) {
					t.Fatalf("case %d: part %q shown in %q", i, part, got)
				}
			}
		}
	}
}

// What the drop moves on past depends only on whole values: a guess at a
// value's start reads the same as other text of its shape, wherever it is.
func TestShellLastLineDropRevealsNothing(t *testing.T) {
	const standIn = "pv-standin-2d6b8f1a"
	redact := func(s string) string { return strings.ReplaceAll(s, standIn, "[secret:STAND_IN]") }
	line := func(out string) string {
		p := &ShellProc{out: &shellRing{max: 1 << 20}}
		_, _ = p.out.Write([]byte(out))
		return p.LastLine(4096, redact, len(standIn))
	}
	for at := 0; at < 40; at++ {
		for _, n := range []int{1, 5, len(standIn) - 1} {
			right, wrong := standIn[:n], strings.Repeat("q", n)
			mk := func(g string) string {
				return strings.Repeat(".", 5000) + strings.Repeat("-", at) + g + strings.Repeat(".", 4096-at-n)
			}
			if a, b := line(mk(right)), line(mk(wrong)); len(a) != len(b) {
				t.Fatalf("at %d, %d bytes: a correct start reads as %d bytes, a wrong one as %d", at, n, len(a), len(b))
			}
		}
	}
}
