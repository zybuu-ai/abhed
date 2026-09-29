package tools_test

import (
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// For any arguments, either the call is refused or policy's subject is the
// value the tool acts on, read from both the canonical and the original bytes.
func FuzzPolicySubjectIsExecutedSubject(f *testing.F) {
	for _, s := range []string{
		`{"command":"echo safe","Command":"touch /tmp/pwned","description":"x"}`,
		`{"command":"a","command":"b","description":"x"}`,
		`{"command":"a","\u0063ommand":"b","description":"x"}`,
		`{"\u0063ommand":"a","description":"x"}`,
		`{"COMMAND":"rm -rf /","description":"x"}`,
		`{"path":"/ok","Path":"/etc/passwd","content":"x"}`,
		`{"command":"ls","path":"/etc/passwd","content":"x"}`,
		`{"pa\u0074h":"/a","content":"x","old_string":"a","new_string":"b"}`,
		`{"\u212A":"a","path":"/a","content":"x"}`,
		`{"path":"/a","offset":1,"PATH":"/b"}`,
		`{"hoſt":"a"}`,
		`{"command":"a"}{"command":"b"}`,
		`{"path":{"path":"x"}}`,
	} {
		f.Add(s)
	}
	reg := []tools.Tool{tools.Bash{}, tools.Write{}, tools.Edit{}, tools.Read{}}
	f.Fuzz(func(t *testing.T, raw string) {
		for _, tool := range reg {
			canon, _, err := tools.CanonicalArgs(tool, json.RawMessage(raw))
			if err != nil {
				continue
			}
			judged := policy.Subject(tool.Name(), canon)
			ran, err := tools.ExecutedSubject(tool.Name(), canon)
			if err != nil {
				continue // the tool refuses it too
			}
			if judged != ran {
				t.Fatalf("%s %q: policy judged %q, tool runs %q", tool.Name(), raw, judged, ran)
			}
			if orig, err := tools.ExecutedSubject(tool.Name(), json.RawMessage(raw)); err == nil && orig != ran {
				t.Fatalf("%s %q: canonical form changed the subject from %q to %q", tool.Name(), raw, orig, ran)
			}
		}
	})
}
