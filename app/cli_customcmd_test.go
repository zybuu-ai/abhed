package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/customcmd"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// customRig is bangRig with a home of its own, the read tool, and no
// managed commands.
func customRig(t *testing.T, answers ...string) (*cliState, *agent.MemStore, *scriptSurface) {
	t.Helper()
	st, store, sf := bangRig(t, answers...)
	st.loop.Tools = tools.NewRegistry(tools.Bash{}, tools.Read{}, tools.Grep{})
	st.appCfg = config.Default()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(config.TrustEnv, "")
	was := customcmd.ManagedDir
	customcmd.ManagedDir = filepath.Join(t.TempDir(), "none")
	t.Cleanup(func() { customcmd.ManagedDir = was; ui.SetCommands(builtinSlash.uiCommands(nil)) })
	return st, store, sf
}

// typeLine runs a line as the prompt does.
func typeLine(t *testing.T, st *cliState, line string) {
	t.Helper()
	dispatchLine(context.Background(), line, ui.NewRenderer(os.Stdout, true), st.loop.Policy, st.sess, st)
}

func userCommand(t *testing.T, name, content string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	write(t, filepath.Join(home, ".abhed", "commands", name), content)
}

// A user command runs: its arguments substituted, recorded as
// command.invoked, and sent as the next turn.
func TestCustomCommandRuns(t *testing.T) {
	st, store, _ := customRig(t)
	userCommand(t, "fix.md", "---\ndescription: fix an issue\n---\nFix issue $1 in $ARGUMENTS")
	st.loop.Recorder.Redact = canaryRedactor{}
	typeLine(t, st, "/fix 42 SECRET-CANARY")
	turn := st.takeTurn()
	if turn == nil || turn.msg.Text != "Fix issue 42 in 42 SECRET-CANARY" {
		t.Fatalf("turn: %+v", turn)
	}
	ev := eventsOf(t, store, agent.EvCommandInvoked)
	if len(ev) != 1 {
		t.Fatalf("%d command.invoked", len(ev))
	}
	var p agent.CommandInvoked
	_ = json.Unmarshal(ev[0].Payload, &p)
	if p.Name != "/fix" || p.Source != "user" || len(p.SHA256) != 64 || strings.Contains(p.Args, "SECRET-CANARY") {
		t.Fatalf("command.invoked %+v", p)
	}
}

// A workspace command does not load until its content is trusted; a change
// after trust needs trust again.
func TestWorkspaceCommandNeedsTrust(t *testing.T) {
	st, store, sf := customRig(t, "yes")
	write(t, filepath.Join(st.sess.Root, ".abhed", "commands", "deploy.md"), "DEPLOY-PROMPT")
	typeLine(t, st, "/deploy")
	if st.takeTurn() != nil || len(eventsOf(t, store, agent.EvCommandInvoked)) != 0 {
		t.Fatal("an untrusted workspace command ran")
	}
	if !strings.Contains(sf.shown(), "not trusted") {
		t.Fatalf("no notice: %q", sf.shown())
	}

	typeLine(t, st, "/commands trust")
	if len(sf.asked) != 1 {
		t.Fatalf("trust asked %d times", len(sf.asked))
	}
	typeLine(t, st, "/deploy")
	if turn := st.takeTurn(); turn == nil || turn.msg.Text != "DEPLOY-PROMPT" {
		t.Fatalf("a trusted command did not run: %+v", turn)
	}

	write(t, filepath.Join(st.sess.Root, ".abhed", "commands", "deploy.md"), "CHANGED-PROMPT")
	st.input.custom = nil // a new session reads the files again
	typeLine(t, st, "/deploy")
	if turn := st.takeTurn(); turn != nil {
		t.Fatalf("a changed command ran on the old trust: %+v", turn)
	}
}

// Declining, or giving no answer, trusts nothing.
func TestWorkspaceCommandTrustNeedsAYes(t *testing.T) {
	for _, answer := range []string{"", "no"} {
		st, _, _ := customRig(t, answer)
		write(t, filepath.Join(st.sess.Root, ".abhed", "commands", "deploy.md"), "DEPLOY")
		typeLine(t, st, "/commands trust")
		typeLine(t, st, "/deploy")
		if st.takeTurn() != nil {
			t.Fatalf("answer %q trusted the command", answer)
		}
	}
}

// A shell line in a command asks, and goes through policy: no answer and a
// deny rule each stop the command.
func TestCustomCommandInlineShellAsks(t *testing.T) {
	st, store, sf := customRig(t, "")
	userCommand(t, "st.md", "status: !`echo INLINE-OUT`")
	typeLine(t, st, "/st")
	if st.takeTurn() != nil || len(sf.asked) != 1 || len(eventsOf(t, store, agent.EvObservation)) != 0 {
		t.Fatalf("an unanswered shell line ran: asked %d", len(sf.asked))
	}

	st, _, _ = customRig(t, "yes")
	userCommand(t, "st.md", "status: !`echo INLINE-OUT`")
	typeLine(t, st, "/st")
	if turn := st.takeTurn(); turn == nil || !strings.Contains(turn.msg.Text, "INLINE-OUT") {
		t.Fatalf("a confirmed shell line's output is missing: %+v", turn)
	}

	st, store, sf = customRig(t, "yes")
	userCommand(t, "v.md", "!`cat my.vault`")
	typeLine(t, st, "/v")
	if st.takeTurn() != nil || len(eventsOf(t, store, agent.EvActionDenied)) != 1 || len(sf.asked) != 0 {
		t.Fatal("a denied shell line in a command was asked about or ran")
	}
}

// allowed-tools narrows the command's turn and nothing after it; a tool the
// session lacks refuses the command.
func TestCustomCommandNarrowsTools(t *testing.T) {
	st, _, _ := customRig(t)
	userCommand(t, "look.md", "---\nallowed-tools: [read]\n---\nlook around")
	userCommand(t, "bad.md", "---\nallowed-tools: [teleport]\n---\nx")
	typeLine(t, st, "/look")
	turn := st.takeTurn()
	if turn == nil || strings.Join(st.loop.Tools.Names(), ",") != "read" {
		t.Fatalf("not narrowed: %v", st.loop.Tools.Names())
	}
	turn.done()
	if len(st.loop.Tools.Names()) != 3 {
		t.Fatalf("not restored: %v", st.loop.Tools.Names())
	}
	typeLine(t, st, "/bad")
	if st.takeTurn() != nil {
		t.Fatal("a command naming a missing tool ran")
	}
}

// A command cannot take a built-in's name.
func TestCustomCommandCannotShadowABuiltin(t *testing.T) {
	st, store, _ := customRig(t)
	userCommand(t, "help.md", "HIJACK")
	typeLine(t, st, "/help")
	if st.takeTurn() != nil || len(eventsOf(t, store, agent.EvCommandInvoked)) != 0 {
		t.Fatal("a custom command replaced /help")
	}
}

// End to end: a command from commands.dirs runs and its text reaches the model.
func TestCLICustomCommand(t *testing.T) {
	c := startCLIPrepared(t, stubReply, stubConfig(`"commands":{"dirs":["cmds"]},`), func(ws string) {
		write(t, filepath.Join(ws, "cmds", "greet.md"), "---\ndescription: greet\n---\nSay hello to $1")
	})
	c.tasks++
	c.command("/greet WORLD-ARG", " in / ")
	c.mu.Lock()
	body := c.bodies[len(c.bodies)-1]
	c.mu.Unlock()
	if !strings.Contains(conversationOf(t, body), "Say hello to WORLD-ARG") {
		t.Fatalf("the command's text did not reach the model: %s", body)
	}
	found := false
	for _, ev := range c.export() {
		found = found || ev.Type == agent.EvCommandInvoked
	}
	if !found {
		t.Fatal("command.invoked is not in the record")
	}
}
