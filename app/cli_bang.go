package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
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
	res, ran := personBash(ctx, st, sf, cmd, "run by the person with !", "")
	if !ran {
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
	st.loop.QueueMessage(agent.Message{Text: bangContext(cmd, out, res.ExitCode)})
}

// personBash runs cmd as the person's bash call and returns its redacted,
// recorded result, or false when it did not run; why has been shown. A
// destructive command always asks. askedBy, when set, names what wants to run
// it, such as a custom command, and then every command asks first: the person
// typed the command's name, not this line.
func personBash(ctx context.Context, st *cliState, sf ui.Surface, cmd, description, askedBy string) (tools.Result, bool) {
	if err := ensureConversation(ctx, st); err != nil {
		sf.Append(ui.Block{Kind: ui.BlockError, Text: "not run: " + err.Error()})
		return tools.Result{}, false
	}
	loop := st.loop
	args := argsJSON(map[string]string{"command": cmd, "description": description})
	d := loop.Policy.Evaluate("bash", true, args)
	destructive := d.Decision == policy.Ask && d.Step == "destructive"
	if askedBy != "" && d.Decision != policy.Deny && !destructive {
		choice, err := sf.Dialog(ctx, ui.DialogSpec{
			Kind:  ui.DialogConfirm,
			Title: askedBy + " wants to run a shell command",
			Body:  []ui.Block{{Kind: ui.BlockToolOut, Text: cmd}},
			Why:   "a command's shell line · asked by " + askedBy,
		})
		if err != nil || choice != ui.ChoiceYes {
			sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "not run: " + cmd})
			return tools.Result{}, false
		}
	}
	id := personCallID("bang")
	tool, refused, confirm, err := loop.ManualAuthorizeTyped(id, args, agent.Unanswered)
	if err == nil && confirm != "" {
		answer := agent.Declined
		choice, derr := sf.Dialog(ctx, ui.DialogSpec{
			Kind:  ui.DialogConfirm,
			Title: "Run this command?",
			Body:  []ui.Block{{Kind: ui.BlockToolOut, Text: cmd}},
			Choices: []ui.Choice{
				{ID: ui.ChoiceYes, Label: "Yes, run it", Destructive: true},
				{ID: ui.ChoiceNo, Label: "No"},
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
		return tools.Result{}, false
	}
	if refused != nil {
		sf.Append(ui.Block{Kind: ui.BlockError, Text: strings.TrimSpace(refused.Content)})
		return tools.Result{}, false
	}
	start := time.Now()
	res := tool.Run(ctx, st.sess, args)
	res.Content = redactFor(loop, res.Content)
	if err := loop.ManualObserve(id, "bash", res, time.Since(start)); err != nil {
		sf.Append(ui.Block{Kind: ui.BlockError, Text: "the output could not be recorded, so it is not used: " + err.Error()})
		return tools.Result{}, false
	}
	return res, true
}

// bangContext is how a ! command and its output join the conversation: the
// model sees what the person ran and what came back, with their next message.
func bangContext(cmd, out string, code *int) string {
	exit := ""
	if code != nil && *code != 0 {
		exit = fmt.Sprintf(" exit=%d", *code)
	}
	return "The person ran a shell command with !. " + untrustedNote + "\n" +
		fenced("bash-input", "", cmd) + "\n" + fenced("bash-output", strings.TrimSpace(exit), out)
}
