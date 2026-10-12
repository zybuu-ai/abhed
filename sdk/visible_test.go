package abhed_test

import (
	"encoding/json"
	"strings"
	"testing"

	abhed "github.com/zybuu-ai/abhed/sdk"
)

// What an embedder shows a person marks the characters that would not print
// as themselves, so an approval prompt cannot be disguised.
func TestVisibleMarksHiddenCharacters(t *testing.T) {
	for _, s := range []string{"rm -rf \u202e/ tsil", "echo \x1b[2Jok", "a\u200bb"} {
		got := abhed.Visible(s)
		if got == s || strings.ContainsAny(got, "\u202e\x1b\u200b") {
			t.Errorf("%q shown as %q", s, got)
		}
	}
	if got := abhed.VisibleLine("one\ntwo"); strings.Contains(got, "\n") {
		t.Errorf("a newline survived VisibleLine: %q", got)
	}
	if abhed.Visible("go test ./...") != "go test ./..." {
		t.Error("plain text changed")
	}
	if !abhed.ArgsHidden(json.RawMessage(`{"command":"ls","note":"\u202e"}`)) || abhed.ArgsHidden(json.RawMessage(`{"command":"ls"}`)) {
		t.Error("ArgsHidden")
	}
}
