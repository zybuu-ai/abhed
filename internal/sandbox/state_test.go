package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The deny for Abhed's own state must come after the workspace allow, because
// in a Seatbelt profile the later rule wins.
func TestSeatbeltProfileShieldsHarnessState(t *testing.T) {
	ws := workspace(t)
	s := NewProcess(Policy{Workspace: ws})
	profile := s.seatbeltProfile()
	allow := strings.Index(profile, `(allow file-write* (subpath "`+ws+`"))`)
	deny := strings.Index(profile, `(deny file-write* (subpath "`+filepath.Join(ws, stateDir)+`"))`)
	read := strings.Index(profile, `(deny file-read* (subpath "`+filepath.Join(ws, stateDir)+`"))`)
	if allow < 0 || deny < 0 || read < 0 || deny < allow {
		t.Fatalf("state directory is not shielded after the workspace allow:\n%s", profile)
	}
}

// A command in the sandbox can neither read the configuration nor plant a
// users file, while the rest of the workspace stays writable.
func TestProcessSandboxShieldsHarnessState(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("state shielding is verified for the Seatbelt backend")
	}
	ws := workspace(t)
	state := filepath.Join(ws, stateDir)
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte(`{"secret":"key-123"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := processSandbox(t, ws, false)

	out, _ := runIn(t, s, ws, "cat .abhed/config.json 2>&1; echo ---; echo planted > .abhed/users.json 2>&1; echo ok > fine.txt; echo done")
	if strings.Contains(out, "key-123") {
		t.Fatalf("the configuration was readable from a command:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(state, "users.json")); err == nil {
		t.Fatalf("a command planted a users file:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "fine.txt")); err != nil {
		t.Fatalf("the workspace itself stopped being writable:\n%s", out)
	}
}
