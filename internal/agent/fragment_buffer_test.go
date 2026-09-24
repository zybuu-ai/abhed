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

// Every split of the reply into two or three fragments emits whole runes,
// never holds a value, lags by under two spans, and adds up to the
// redacted reply, including where a label is shorter or longer than its value
// and where a value starts the way a label does.
func TestFragmentBufferEverySplit(t *testing.T) {
	cases := []struct {
		vals  map[string]string
		reply string
	}{
		{map[string]string{"A": "ab", "B": "sk-0123456789abcdefghij", "C": `quo"te/é`},
			`x sk-0123456789abcdefghij é ab quo"te/é ✓ ab`},
		{map[string]string{"BRACKET": "[abcdefghijkl"}, "see [abcdefghijkl now"},
		{map[string]string{"LABELLIKE": "[secret:k9zz", "IPV6": "[fe80::1]:22"},
			"a [secret:k9zz then some text [fe80::1]:22 and more [secret:k9zz"},
	}
	for _, tc := range cases {
		want := redactedText(bufferFor(t, tc.vals).redact, tc.reply)
		span := bufferFor(t, tc.vals).span
		cuts := runeCuts(tc.reply)
		for i, a := range cuts {
			for _, b := range cuts[i:] {
				buf := bufferFor(t, tc.vals)
				var out strings.Builder
				for _, frag := range []string{tc.reply[:a], tc.reply[a:b], tc.reply[b:]} {
					got := buf.push(frag)
					if !utf8.ValidString(got) {
						t.Fatalf("cut %d,%d: emitted a split rune %q", a, b, got)
					}
					if len(buf.carry) >= 2*span+utf8.UTFMax {
						t.Fatalf("cut %d,%d: held back %d bytes with a span of %d", a, b, len(buf.carry), span)
					}
					out.WriteString(got)
					for _, v := range tc.vals {
						if len(v) > 2 && strings.Contains(out.String(), v) {
							t.Fatalf("cut %d,%d: %q was emitted", a, b, v)
						}
					}
				}
				out.WriteString(buf.push("") + buf.flush())
				if out.String() != want {
					t.Fatalf("%q cut %d,%d: got %q, want %q", tc.reply, a, b, out.String(), want)
				}
			}
		}
	}
}

// A redactor that breaks the JSON it is given withholds the text: nothing is
// emitted while streaming, and the flush gives the marker, not the text.
func TestFragmentBufferWithholdsWhatCannotBeRedacted(t *testing.T) {
	b := &fragmentBuffer{redact: breakJSON, span: 8}
	var out strings.Builder
	for _, frag := range []string{"the value is ", "hunter22 ", "and more text"} {
		out.WriteString(b.push(frag))
	}
	out.WriteString(b.flush())
	if out.String() != Withheld {
		t.Fatalf("got %q, want only the marker", out.String())
	}
}

func breakJSON(b []byte) []byte { return append([]byte(nil), b[:len(b)-1]...) }

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
