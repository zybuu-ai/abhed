package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -p with json or stream-json keeps stdout to events, which carry
// secrets_withheld, and tells the person on stderr, as text does.
func TestPrintJSONSaysOnStderrWhatWasWithheld(t *testing.T) {
	for _, format := range []string{"json", "stream-json"} {
		m := &bgModelServer{}
		ws := bgWorkspace(t, m.start(t), "")
		vault := filepath.Join(os.Getenv("HOME"), ".abhed", "secrets.json")
		if err := os.WriteFile(vault, []byte(`{"API_TOKEN":"tok-5e6f7a-cli"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := mainHelper([]string{"-C", ws, "-output-format", format, "-p", "use tok-5e6f7a-cli"})
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v\n%s\n%s", format, err, out.String(), errOut.String())
		}
		if !strings.Contains(errOut.String(), "abhed: your message held the value of the stored secret API_TOKEN") {
			t.Errorf("%s: no note on stderr:\n%s", format, errOut.String())
		}
		withheld := false
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var ev struct {
				Type    string `json:"type"`
				Payload struct {
					SecretsWithheld string `json:"secrets_withheld"`
				} `json:"payload"`
			}
			if json.Unmarshal([]byte(line), &ev) != nil {
				t.Fatalf("%s: stdout holds a line that is not JSON: %q", format, line)
			}
			withheld = withheld || ev.Type == "user.message" && ev.Payload.SecretsWithheld == "API_TOKEN"
		}
		if !withheld || strings.Contains(out.String(), "tok-5e6f7a-cli") {
			t.Errorf("%s: stdout:\n%s", format, out.String())
		}
	}
}
