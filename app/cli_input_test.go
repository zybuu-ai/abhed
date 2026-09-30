package app

import (
	"context"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Until mentions are built, a line goes to the model exactly as typed and
// nothing is read: an @ is text, not a file.
func TestPlainInputSendsTheLineAsTyped(t *testing.T) {
	for _, raw := range []string{"fix it", "look at @~/.ssh/id_rsa", "!rm -rf build", "# note"} {
		msg, att, err := plainInput{}.Expand(context.Background(), nil, raw)
		if err != nil || msg.Text != raw || len(att) != 0 {
			t.Fatalf("%q became %+v, %v, %v", raw, msg, att, err)
		}
	}
	a := Attachment{Path: "a.go", Range: "1-2", SHA256: "ab", Bytes: 9, Truncated: true}
	if a.Mention() != (agent.InputMention{Path: "a.go", Range: "1-2", SHA256: "ab", Bytes: 9, Truncated: true}) {
		t.Fatalf("mention %+v", a.Mention())
	}
}
