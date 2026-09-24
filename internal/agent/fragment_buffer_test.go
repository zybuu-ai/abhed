package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/secrets"
)

func bufferFor(t *testing.T, vals map[string]string) *fragmentBuffer {
	t.Helper()
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	for name, v := range vals {
		if err := vault.Set(name, v); err != nil {
			t.Fatal(err)
		}
	}
	l := &Loop{Recorder: &Recorder{Redact: vault.Redactor()}}
	return l.fragments()
}

// Every split of the reply into three fragments emits text that is whole
// runes, never holds a value, lags by under two spans, and adds up to the
// redacted reply, including where a label is shorter or longer than its value.
func TestFragmentBufferEverySplit(t *testing.T) {
	vals := map[string]string{
		"A": "ab",
		"B": "sk-0123456789abcdefghij",
		"C": `quo"te/é`,
	}
	reply := `x sk-0123456789abcdefghij é ab quo"te/é ✓ ab`
	want := strings.NewReplacer(vals["B"], "[secret:B]", vals["C"], "[secret:C]", vals["A"], "[secret:A]").Replace(reply)
	span := bufferFor(t, vals).span
	cuts := runeCuts(reply)
	for i, a := range cuts {
		for _, b := range cuts[i+1:] {
			buf := bufferFor(t, vals)
			var out strings.Builder
			for _, frag := range []string{reply[:a], reply[a:b], reply[b:]} {
				got := buf.push(frag)
				if !utf8.ValidString(got) {
					t.Fatalf("cut %d,%d: emitted a split rune %q", a, b, got)
				}
				if len(buf.carry) >= 2*span+utf8.UTFMax {
					t.Fatalf("cut %d,%d: held back %d bytes with a span of %d", a, b, len(buf.carry), span)
				}
				out.WriteString(got)
				for _, v := range vals {
					if len(v) > 2 && strings.Contains(out.String(), v) {
						t.Fatalf("cut %d,%d: %q was emitted", a, b, v)
					}
				}
			}
			out.WriteString(buf.push("") + buf.flush())
			if out.String() != want {
				t.Fatalf("cut %d,%d: got %q, want %q", a, b, out.String(), want)
			}
		}
	}
}

// The span is the longest value as a payload holds it, escapes included.
func TestFragmentBufferSpan(t *testing.T) {
	if got := bufferFor(t, nil).span; got != 0 {
		t.Fatalf("no secrets, span %d", got)
	}
	// The payload form of a"b< is a\"b\u003c: ten bytes, not four.
	if got := bufferFor(t, map[string]string{"A": "abc", "B": `a"b<`}).span; got != 10 {
		t.Fatalf("span %d, want the escaped length 10", got)
	}
	if got := bufferFor(t, nil).push("as is"); got != "as is" {
		t.Fatalf("with no secrets a fragment changed: %q", got)
	}
}
