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
	c := startCLIPrepared(t, stubReply, stubConfig(`"commands":{"dirs":["cmds"]},`), func(ws, _ string) {
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

// A relative commands.dirs entry in the person's own configuration resolves
// into the workspace: those commands came with the repository and need its
// trust, as .abhed/commands does.
func TestCommandsDirInsideWorkspaceNeedsTrust(t *testing.T) {
	st, store, _ := customRig(t, "yes")
	st.appCfg.Commands.Dirs = []string{"tools/cmds"}
	write(t, filepath.Join(st.sess.Root, "tools", "cmds", "plant.md"), "PLANTED")
	typeLine(t, st, "/plant")
	if st.takeTurn() != nil || len(eventsOf(t, store, agent.EvCommandInvoked)) != 0 {
		t.Fatal("a command in the workspace ran without trust")
	}
	// An absolute path into the workspace, or through a link to it, is the same.
	st, _, _ = customRig(t)
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(st.sess.Root, link); err != nil {
		t.Skip(err)
	}
	st.appCfg.Commands.Dirs = []string{filepath.Join(link, "cmds")}
	write(t, filepath.Join(st.sess.Root, "cmds", "plant.md"), "PLANTED")
	typeLine(t, st, "/plant")
	if st.takeTurn() != nil {
		t.Fatal("a command reached through a link into the workspace ran without trust")
	}
	// Once trusted, it runs.
	st, _, _ = customRig(t, "yes")
	st.appCfg.Commands.Dirs = []string{"tools/cmds"}
	write(t, filepath.Join(st.sess.Root, "tools", "cmds", "plant.md"), "PLANTED")
	typeLine(t, st, "/commands trust")
	typeLine(t, st, "/plant")
	if turn := st.takeTurn(); turn == nil || turn.msg.Text != "PLANTED" {
		t.Fatalf("a trusted command did not run: %+v", turn)
	}
}

// Two commands queued during a turn run one after another: the second is
// refused while the first's turn waits, so the first keeps its turn and its
// restore, and the tools come back when that turn ends.
func TestQueuedCustomCommandsDoNotClobberTheWaitingTurn(t *testing.T) {
	st, store, _ := customRig(t)
	userCommand(t, "look.md", "---\nallowed-tools: [read]\n---\nLOOK")
	userCommand(t, "grep.md", "---\nallowed-tools: [grep]\n---\nGREP")
	typeLine(t, st, "/look")
	typeLine(t, st, "/grep")
	typeLine(t, st, "/init")
	turn := st.takeTurn()
	if turn == nil || turn.msg.Text != "LOOK" || strings.Join(st.loop.Tools.Names(), ",") != "read" {
		t.Fatalf("turn %+v tools %v", turn, st.loop.Tools.Names())
	}
	if n := len(eventsOf(t, store, agent.EvCommandInvoked)); n != 1 {
		t.Fatalf("%d commands recorded; the refused ones must not be", n)
	}
	turn.done()
	if len(st.loop.Tools.Names()) != 3 {
		t.Fatalf("tools not restored: %v", st.loop.Tools.Names())
	}
}

// A shell line's output that names a file is not read as the person's
// mention; the command's own @ files still attach.
func TestCustomCommandOutputIsNotReadAsAMention(t *testing.T) {
	st, store, _ := customRig(t, "yes")
	write(t, filepath.Join(st.sess.Root, "own.md"), "OWN-FILE")
	write(t, filepath.Join(st.sess.Root, "other.md"), "OTHER-CANARY")
	userCommand(t, "c.md", "see @own.md and !`echo @other.md`")
	typeLine(t, st, "/c")
	turn := st.takeTurn()
	if turn == nil || !strings.Contains(turn.msg.Text, "OWN-FILE") || strings.Contains(turn.msg.Text, "OTHER-CANARY") ||
		!strings.Contains(turn.msg.Text, "@other.md") {
		t.Fatalf("turn: %+v", turn)
	}
	if n := len(eventsOf(t, store, agent.EvInputMention)); n != 1 {
		t.Fatalf("%d mentions", n)
	}
}

// An unanswered trust question decides nothing, so it is asked again.
func TestUnansweredCommandTrustIsNotStored(t *testing.T) {
	st, _, _ := customRig(t, "")
	write(t, filepath.Join(st.sess.Root, ".abhed", "commands", "deploy.md"), "DEPLOY")
	typeLine(t, st, "/commands trust")
	store, err := commandTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, why := store.Decide(st.sess.Root, st.input.custom.wsSum); why != "new" {
		t.Fatalf("an unanswered question was stored as %s", why)
	}
}

// When the workspace is the home directory, or holds it, a relative
// commands.dirs entry is still the workspace's and needs trust; the
// person's own ~/.abhed/commands still loads.
func TestRelativeCommandsDirAtHomeNeedsTrust(t *testing.T) {
	for _, above := range []bool{false, true} {
		st, store, _ := customRig(t)
		home := st.sess.Root
		if above {
			home = filepath.Join(st.sess.Root, "users", "me")
		}
		t.Setenv("HOME", home)
		st.appCfg.Commands.Dirs = []string{"cmds"}
		write(t, filepath.Join(st.sess.Root, "cmds", "plant.md"), "PLANTED")
		userCommand(t, "mine.md", "MINE")
		typeLine(t, st, "/plant")
		if st.takeTurn() != nil || len(eventsOf(t, store, agent.EvCommandInvoked)) != 0 {
			t.Fatalf("above=%v: a relative commands dir ran without trust", above)
		}
		typeLine(t, st, "/mine")
		if turn := st.takeTurn(); turn == nil || turn.msg.Text != "MINE" {
			t.Fatalf("above=%v: the person's own command did not run: %+v", above, turn)
		}
	}
}
