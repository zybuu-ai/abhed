//go:build unix

package clitest

import (
	"path/filepath"
	"strings"
	"testing"
)

const statuslineConfig = `{"sandbox":{"min_tier":"none"},"statusline":{"command":"grep -o '\"provider\":\"[a-z]*\"'; printf '\\033]0;pwned\\007'"},` +
	`"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"{{MODEL_URL}}","model":"stub-model","context_window":32768}}}}`

// The user's statusline command reads the status as JSON and its output is
// shown after each task, with no escape but colour reaching the terminal.
func TestStatuslineFromUserConfig(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{UserConfig: statuslineConfig, Cols: 120, Script: `text "done"`})
	h.WaitText("Type a task")
	h.Type("hi\r")
	h.WaitText(`"provider":"stub"`)
	if h.Screen().Title() == "pwned" {
		t.Fatal("the statusline set the terminal title")
	}
	h.Type("/status\r")
	h.WaitText("statusline")
	h.Exit(0)
}

// A workspace's statusline is a process: untrusted, it never runs.
func TestStatuslineFromUntrustedWorkspaceIgnored(t *testing.T) {
	t.Parallel()
	h := StartRun(t, Opts{Cols: 120, Script: `text "done"`, Setup: func(home, ws string) {
		writeFile(t, filepath.Join(ws, ".abhed", "config.json"), `{"statusline":{"command":"echo from-the-repo"}}`)
	}})
	h.WaitText("Trust this file?")
	h.Type("d\n")
	h.WaitText("Type a task")
	h.Type("hi\r")
	h.WaitText("done")
	h.WaitText("turns")
	for _, line := range strings.Split(Strip(h.Output()), "\n") {
		if strings.TrimSpace(line) != "from-the-repo" {
			continue
		}
		t.Fatal("an untrusted workspace's statusline ran")
	}
	h.Exit(0)
}
