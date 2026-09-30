package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// Limits on a ! line's output: what is shown at once, and what joins the
// next message.
const (
	bangShownLines = 40
	bangOutputMax  = 30000
)

// runBang runs a line typed after ! as the person's own bash call. It goes
// through the bash tool, so the policy (a deny rule holds, plan mode refuses),
// the sandbox and the record apply as to the agent's commands. It is never
// pre-approved: a destructive command asks, and no answer refuses it. The
// output is shown and joins the next message, redacted and capped.
func runBang(ctx context.Context, st *cliState, r *ui.Renderer, cmd string) {
	sf := surfaceOf(st, r)
	if cmd == "" {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "! runs a shell command: !git status"})
		return
	}
	if err := ensureConversation(ctx, st); err != nil {
		sf.Append(ui.Block{Kind: ui.BlockError, Text: "not run: " + err.Error()})
		return
	}
	loop := st.loop
	args := argsJSON(map[string]string{"command": cmd, "description": "run by the person with !"})
	id := personCallID("bang")
	tool, refused, confirm, err := loop.ManualAuthorizeTyped(id, args, agent.Unanswered)
	if err == nil && confirm != "" {
		answer := agent.Declined
		choice, derr := sf.Dialog(ctx, ui.DialogSpec{
			Kind:  ui.DialogConfirm,
			Title: "Run this command?",
			Body:  []ui.Block{{Kind: ui.BlockToolOut, Text: cmd}},
			Choices: []ui.Choice{
				{ID: ui.ChoiceYes, Label: "Yes, run it", Key: 'y', Destructive: true},
				{ID: ui.ChoiceNo, Label: "No", Key: 'n'},
			},
			Why: "destructive · " + confirm + " · asked by policy",
		})
		if derr == nil && choice == ui.ChoiceYes {
			answer = agent.Confirmed
		}
		tool, refused, _, err = loop.ManualAuthorizeTyped(id, args, answer)
	}
	if err != nil {
		sf.Append(ui.Block{Kind: ui.BlockError, Text: "not run: " + err.Error()})
		return
	}
	if refused != nil {
		sf.Append(ui.Block{Kind: ui.BlockError, Text: strings.TrimSpace(refused.Content)})
		return
	}
	start := time.Now()
	res := tool.Run(ctx, st.sess, args)
	res.Content = redactFor(loop, res.Content)
	if err := loop.ManualObserve(id, "bash", res, time.Since(start)); err != nil {
		sf.Append(ui.Block{Kind: ui.BlockError, Text: "the output could not be recorded, so it is not added: " + err.Error()})
		return
	}
	out := strings.TrimRight(res.Content, "\n")
	lines := strings.Split(out, "\n")
	shown := lines
	if len(shown) > bangShownLines {
		shown = shown[:bangShownLines]
	}
	text := strings.Join(shown, "\n")
	if len(lines) > len(shown) {
		text += fmt.Sprintf("\n… +%d lines (all of it goes to the agent with your next message)", len(lines)-len(shown))
	}
	sf.Append(ui.Block{Kind: ui.BlockToolOut, Text: text})
	if len(out) > bangOutputMax {
		out, _ = capText(out, bangOutputMax)
		out += "\n[output truncated]"
	}
	loop.QueueMessage(agent.Message{Text: bangContext(cmd, out, res.ExitCode)})
}

// bangContext is how a ! command and its output join the conversation: the
// model sees what the person ran and what came back, with their next message.
func bangContext(cmd, out string, code *int) string {
	exit := ""
	if code != nil && *code != 0 {
		exit = fmt.Sprintf("\n<bash-exit-code>%d</bash-exit-code>", *code)
	}
	return fmt.Sprintf("The person ran a shell command with !:\n<bash-input>%s</bash-input>\n<bash-output>%s</bash-output>%s", cmd, out, exit)
}
