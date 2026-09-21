package server

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// applyUnified rebuilds the new text from the old text and a diff, checking
// every context and removed line against the old text on the way. A diff that
// passes describes the change exactly; nothing else about it needs trusting.
func applyUnified(t *testing.T, before, diff string) string {
	t.Helper()
	old := splitLines(before)
	var out strings.Builder
	at := 0
	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	for i := 2; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "@@") {
			var start, count int
			if _, err := fmt.Sscanf(line, "@@ -%d,%d", &start, &count); err != nil {
				t.Fatalf("bad hunk header %q: %v", line, err)
			}
			if count > 0 {
				start--
			}
			for ; at < start; at++ {
				out.WriteString(old[at])
			}
			continue
		}
		text := line[1:] + "\n"
		if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "\\") {
			text = line[1:]
		}
		switch line[0] {
		case ' ', '-':
			if at >= len(old) || old[at] != text {
				t.Fatalf("diff line %d %q does not match the old text", i, line)
			}
			if line[0] == ' ' {
				out.WriteString(text)
			}
			at++
		case '+':
			out.WriteString(text)
		case '\\':
		default:
			t.Fatalf("unexpected diff line %q", line)
		}
	}
	for ; at < len(old); at++ {
		out.WriteString(old[at])
	}
	return out.String()
}

func TestUnifiedDiffDescribesTheChangeExactly(t *testing.T) {
	cases := [][2]string{
		{"", "new\n"},
		{"gone\n", ""},
		{"a\nb\nc\n", "a\nB\nc\n"},
		{"a\nb\nc", "a\nb\nc\n"},
		{"a\nb\nc\n", "a\nb\nc"},
		{"same\n", "same\n"},
	}
	rng := rand.New(rand.NewSource(1))
	for n := 0; n < 200; n++ {
		var a, b []string
		for i := 0; i < rng.Intn(60); i++ {
			a = append(a, strconv.Itoa(rng.Intn(8)))
		}
		for _, line := range a {
			switch rng.Intn(6) {
			case 0:
			case 1:
				b = append(b, line, strconv.Itoa(rng.Intn(8)))
			default:
				b = append(b, line)
			}
		}
		cases = append(cases, [2]string{strings.Join(a, "\n") + "\n", strings.Join(b, "\n") + "\n"})
	}

	for _, c := range cases {
		diff, added, removed := unifiedDiff("f", c[0], c[1])
		if got := applyUnified(t, c[0], diff); got != c[1] {
			t.Fatalf("applying the diff of %q -> %q gave %q\n%s", c[0], c[1], got, diff)
		}
		if plus := strings.Count(diff, "\n+") - 1; plus != added {
			t.Fatalf("added = %d, but the diff has %d + lines\n%s", added, plus, diff)
		}
		if minus := strings.Count("\n"+diff, "\n-") - 1; minus != removed {
			t.Fatalf("removed = %d, but the diff has %d - lines\n%s", removed, minus, diff)
		}
	}
}

func TestUnifiedDiffKeepsDistantEditsInSeparateHunks(t *testing.T) {
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, "line "+strconv.Itoa(i))
	}
	before := strings.Join(lines, "\n") + "\n"
	lines[1], lines[25] = "LINE 2", "LINE 26"
	after := strings.Join(lines, "\n") + "\n"

	diff, _, _ := unifiedDiff("f", before, after)
	if !strings.Contains(diff, "@@ -1,5 +1,5 @@") || !strings.Contains(diff, "@@ -23,7 +23,7 @@") {
		t.Fatalf("want two hunks with three lines of context:\n%s", diff)
	}
}

// Past the search bound the diff falls back to old-then-new. It must still be
// a correct diff, and it must not take the memory an unbounded search would.
func TestUnifiedDiffOfARewriteIsStillCorrect(t *testing.T) {
	var a, b []string
	for i := 0; i < 3*maxEditDistance; i++ {
		a = append(a, "old "+strconv.Itoa(i))
		b = append(b, "new "+strconv.Itoa(i))
	}
	before := "head\n" + strings.Join(a, "\n") + "\ntail\n"
	after := "head\n" + strings.Join(b, "\n") + "\ntail\n"
	diff, added, removed := unifiedDiff("f", before, after)
	if got := applyUnified(t, before, diff); got != after {
		t.Fatal("the fallback diff does not reproduce the new text")
	}
	if added != len(b) || removed != len(a) {
		t.Fatalf("added %d removed %d", added, removed)
	}
}
