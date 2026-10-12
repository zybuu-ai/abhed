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
	for _, s := range []string{"rm -rf ‮/ tsil", "echo \x1b[2Jok", "a​b"} {
		got := abhed.Visible(s)
		if got == s || strings.ContainsAny(got, "‮\x1b​") {
			t.Errorf("%q shown as %q", s, got)
		}
	}
	if got := abhed.VisibleLine("one\ntwo"); strings.Contains(got, "\n") {
		t.Errorf("a newline survived VisibleLine: %q", got)
	}
	if abhed.Visible("go test ./...") != "go test ./..." {
		t.Error("plain text changed")
	}
	if !abhed.ArgsHidden(json.RawMessage(`{"command":"ls","note":"‮"}`)) || abhed.ArgsHidden(json.RawMessage(`{"command":"ls"}`)) {
		t.Error("ArgsHidden")
	}
}
