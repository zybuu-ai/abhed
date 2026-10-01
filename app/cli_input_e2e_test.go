package app

// Terminal end-to-end scenarios for interactive input, on the pty harness.
// The mention symlink case also runs over a pipe (TestCLIMentionReachesModelThroughPolicy),
// and the untrusted workspace command and ! under a deny rule run in process
// (TestWorkspaceCommandNeedsTrust, TestBangUnderDenyRuleRefusedAndRecorded).

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/clitest"
)

func e2eWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	return ws
}

// S5: a mention through a link out of the workspace is refused, and nothing
// of the file reaches the model or the record.
func TestE2EMentionSymlinkRefused(t *testing.T) {
	ws, outside := e2eWorkspace(t), t.TempDir()
	write(t, filepath.Join(outside, "id_rsa"), "PRIVATE-KEY-CANARY")
	if err := os.Symlink(outside, filepath.Join(ws, "docs")); err != nil {
		t.Skip(err)
	}
	h := startAtPrompt(t, clitest.Opts{Script: `text "ok"`, Args: []string{"-C", ws}})
	h.Type("read @docs/id_rsa")
	h.Key(clitest.Enter)
	h.WaitText("not sent")
	for _, r := range h.Requests() {
		if strings.Contains(string(r.Body), "CANARY") {
			t.Fatal("a file outside the workspace reached the model")
		}
	}
	if slices.Contains(h.Record().Types(), agent.EvInputMention) {
		t.Fatal("a refused mention was recorded as attached")
	}
	h.Exit(0)
}

// A command that came with the workspace does not run until it is trusted.
func TestE2EUntrustedWorkspaceCommand(t *testing.T) {
	ws := e2eWorkspace(t)
	write(t, filepath.Join(ws, ".abhed", "commands", "deploy.md"), "DEPLOY-PROMPT")
	h := startAtPrompt(t, clitest.Opts{Script: `text "ok"`, Args: []string{"-C", ws}})
	h.Type("/deploy")
	h.Key(clitest.Enter)
	h.WaitText("not trusted")
	if len(h.Requests()) != 0 || slices.Contains(h.Record().Types(), agent.EvCommandInvoked) {
		t.Fatal("an untrusted workspace command ran")
	}
	h.Exit(0)
}

// ! under a deny rule is refused, and the refusal is in the record as the
// person's denied call.
func TestE2EBangDenied(t *testing.T) {
	ws := e2eWorkspace(t)
	h := startAtPrompt(t, clitest.Opts{Script: `text "ok"`, Args: []string{"-C", ws},
		Managed: `{"permissions":{"deny":["bash(cat *vault*)"]}}`})
	h.Type("!cat my.vault")
	h.Key(clitest.Enter)
	h.WaitText("bash(cat *vault*)")
	if !slices.Contains(h.Record().Types(), agent.EvActionDenied) {
		t.Fatal("the refusal is not in the record")
	}
	h.Exit(0)
}

// startAtPrompt starts the CLI on the pty and waits for its prompt: keys
// typed before the editor takes the terminal arrive cooked.
func startAtPrompt(t *testing.T, o clitest.Opts) clitest.Harness {
	t.Helper()
	h := clitest.Start(t, o)
	h.WaitText("Type a task")
	h.Settle()
	return h
}
