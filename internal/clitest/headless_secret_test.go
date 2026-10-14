//go:build unix

package clitest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// abhed -p "… <a stored value> …": the model gets [secret:NAME], stdout keeps
// to the answer, and stderr says, once, which secret was withheld.
func TestHeadlessWithholdsATypedSecret(t *testing.T) {
	t.Parallel()
	const value = "tok-9f8e7d-cli"
	h := piped(t, Opts{Args: []string{"-p", "log in with " + value}, Script: `text "done"`,
		Setup: func(home, _ string) {
			if err := os.WriteFile(filepath.Join(home, ".abhed", "secrets.json"), []byte(`{"API_TOKEN":"`+value+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}})
	req := lastRequestText(t, h)
	if strings.Contains(req, value) || !strings.Contains(req, "log in with [secret:API_TOKEN]") {
		t.Fatalf("the model was sent:\n%s", req)
	}
	if got := h.Stderr(); strings.Count(got, "not sent to the model") != 1 || !strings.Contains(got, "stored secret API_TOKEN") {
		t.Fatalf("stderr:\n%s", got)
	}
	if strings.Contains(h.Stdout(), "not sent to the model") || !strings.Contains(h.Stdout(), "done") {
		t.Fatalf("stdout:\n%s", h.Stdout())
	}
}
