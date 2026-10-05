package visible

import "testing"

// A space other than ' ' is shown in a field a person approves, alone or in
// a run; in output, where it is only text, it stays.
func TestOtherSpacesAreShown(t *testing.T) {
	for in, want := range map[string]string{
		"rm\u00a0-rf /":              "rm⟨U+00A0⟩-rf /",
		"a\u00a0\u00a0\u00a0\u00a0b": "a⟨U+00A0⟩⟨U+00A0⟩⟨U+00A0⟩⟨U+00A0⟩b",
		"x\u2003y\u3000z":            "x⟨U+2003⟩y⟨U+3000⟩z",
		"plain words":                "plain words",
	} {
		if got := Text(in, false, true); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Text("prix\u00a0: 5", true, false); got != "prix\u00a0: 5" {
		t.Errorf("output changed: %q", got)
	}
}
