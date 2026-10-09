package app

import (
	"bytes"
	"strings"
	"testing"
)

// abhed sandbox is refused inside an agent's command, every verb of it,
// before any profile is read.
func TestSandboxCmdRefusedInAgent(t *testing.T) {
	t.Setenv("ABHED_PROFILES_FILE", t.TempDir()+"/profiles.json")
	was := inAgentCommand
	inAgentCommand = func() string { return "ABHED_SANDBOX is set" }
	t.Cleanup(func() { inAgentCommand = was })
	for _, args := range [][]string{{"list"}, {"connect", "--stdio", "sbx_0123456789abcdef"}, {"template", "list"}} {
		var errOut bytes.Buffer
		if code := sandboxCmd(t.TempDir(), args, strings.NewReader(""), &bytes.Buffer{}, &errOut); code != 1 ||
			!strings.Contains(errOut.String(), "refused inside an agent's command") {
			t.Errorf("%v: %d %q", args, code, errOut.String())
		}
	}
}

func TestSandboxCmdUsage(t *testing.T) {
	t.Setenv("ABHED_PROFILES_FILE", t.TempDir()+"/profiles.json")
	var errOut bytes.Buffer
	for _, args := range [][]string{nil, {"nope"}, {"connect"}, {"exec", "sbx_0123456789abcdef"}} {
		errOut.Reset()
		if code := sandboxCmd(t.TempDir(), args, strings.NewReader(""), &bytes.Buffer{}, &errOut); code != 2 {
			t.Errorf("%v: %d %q", args, code, errOut.String())
		}
	}
	errOut.Reset()
	if code := sandboxCmd(t.TempDir(), []string{"-h"}, strings.NewReader(""), &bytes.Buffer{}, &errOut); code != 0 ||
		!strings.Contains(errOut.String(), "ssh-config") {
		t.Fatalf("help: %d %q", code, errOut.String())
	}
}
