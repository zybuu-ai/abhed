package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/review", Args: "[base]", Help: "review the current diff, read-only in plan mode",
		Group: "context", Order: 96, Run: func(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
			return false, runReview(ctx, e, "/review", reviewPrompt, args)
		}})
	registerSlash(slashCmd{Name: "/security-review", Args: "[base]", Help: "security review of the current diff, read-only in plan mode",
		Group: "context", Order: 97, Run: func(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
			return false, runReview(ctx, e, "/security-review", securityReviewPrompt, args)
		}})
}

// reviewPrompt and securityReviewPrompt ship in the binary, so the record's
// sha256 names exactly what the model was asked.
const reviewPrompt = `Review the code change below as a careful senior engineer would before it merges.
This is a read-only review: you are in plan mode, so change nothing and do not propose a plan to carry out.
You may read files in the workspace for context.

Report, most important first:
1. Correctness bugs: wrong logic, missed edge cases, broken error handling, races.
2. Tests: behaviour the change adds or alters that no test would catch.
3. Maintainability: unclear names, duplication, dead code, comments that no longer match the code.
For each finding give the file and line, what is wrong, why it matters, and a concrete fix.
Say plainly when a finding is uncertain. If the change looks right, say so and stop; do not invent findings.`

const securityReviewPrompt = `Do a security review of the code change below, as an application security engineer would before it merges.
This is a read-only review: you are in plan mode, so change nothing and do not propose a plan to carry out.
You may read files in the workspace for context.

Look for, at least: injection (shell, SQL, path, template), missing or wrong authorisation, secrets in code or logs,
unsafe deserialisation, path traversal and symlink following, untrusted input reaching a sink unchecked,
weakened cryptography or randomness, permission or sandbox checks that can be bypassed, and denial of service.
For each finding give the file and line, how an attacker would reach it, its impact, a severity
(critical, high, medium, low), and a concrete fix. Report only issues the change introduces or exposes.
Say plainly when a finding is uncertain. If you find nothing, say so; do not invent findings.`

// reviewBase is a revision a review may compare against: no option, no
// shell syntax, so it is safe as one word of the git command.
var reviewBase = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./~^@{}-]*$`)

// reviewDiffCommand is the command that reads the diff. External diff and
// textconv drivers are off: they are programs the repository's config names.
func reviewDiffCommand(base string) string {
	if base == "" {
		base = "HEAD"
	}
	return "git --no-pager status --short && git --no-pager diff --no-ext-diff --no-textconv " + base
}

// runReview reads the diff as the person's own policed, recorded bash call,
// then sends prompt and diff as the next turn in plan mode. The mode the
// session had comes back when that turn ends.
func runReview(ctx context.Context, e *cmdEnv, name, prompt string, args []string) error {
	st, sf := e.st, e.ui
	if len(args) > 1 {
		return fmt.Errorf("usage: %s [base]", name)
	}
	base := ""
	if len(args) == 1 {
		base = args[0]
		if !reviewBase.MatchString(base) {
			return fmt.Errorf("%s is not a revision %s can compare against", base, name)
		}
	}
	if err := st.turnFree(); err != nil {
		return err
	}
	if e.pol != nil && e.pol.Mode == policy.ModePlan {
		// Plan mode refuses every shell command, git diff included.
		return fmt.Errorf("plan mode refuses the shell command that reads the diff; leave plan mode (/mode default) and run %s again: it runs in plan mode itself", name)
	}
	if err := ensureConversation(ctx, st); err != nil {
		return err
	}
	cmd := reviewDiffCommand(base)
	res, ran := personBash(ctx, st, sf, cmd, "read the diff for "+name, "")
	if !ran {
		return fmt.Errorf("%s was not sent: the diff could not be read", name)
	}
	// The bash tool heads its output with the exit line; the review wants the diff alone.
	out := res.Content
	if _, body, ok := strings.Cut(out, "\n"); ok && strings.HasPrefix(out, "exit ") {
		out = body
	}
	out = strings.TrimRight(out, "\n")
	if res.ExitCode != nil && *res.ExitCode != 0 {
		return fmt.Errorf("%s was not sent: %s failed: %s", name, cmd, firstLine(strings.TrimSpace(out), 200))
	}
	if t := strings.TrimSpace(out); t == "" || t == "[no output]" {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "no changes to review"})
		return nil
	}
	if len(out) > bangOutputMax {
		out, _ = capText(out, bangOutputMax)
		out += "\n[diff truncated; read the files for the rest]"
	}
	sum := sha256.Sum256([]byte(prompt))
	if _, err := st.loop.Recorder.Record(agent.EvCommandInvoked, agent.ActorUser, agent.Trusted, agent.CommandInvoked{
		Name: name, Source: sourceBuiltin, SHA256: hex.EncodeToString(sum[:]), Args: base,
	}); err != nil {
		return err
	}

	restore, err := reviewInPlanMode(ctx, e)
	if err != nil {
		return err
	}
	text := prompt + "\n\n" + untrustedNote + "\n" + fenced("diff", "", out)
	if err := st.sendTurn(agent.Message{Text: text}, restore); err != nil {
		restore()
		return err
	}
	return nil
}

// reviewInPlanMode moves the session to plan mode for the review's turn and
// returns what puts the earlier mode back. Bypass is chosen only at startup,
// so after a review from bypass the session stays in plan mode.
func reviewInPlanMode(ctx context.Context, e *cmdEnv) (func(), error) {
	if e.modes == nil || e.pol == nil {
		return nil, errors.New("this session cannot change mode, so the review cannot run read-only")
	}
	was := e.pol.Mode
	if err := e.modes.Set(ctx, policy.ModePlan, agent.ViaSlash); err != nil {
		return nil, fmt.Errorf("the review needs plan mode: %w", err)
	}
	return func() {
		if was == policy.ModePlan || e.pol.Mode != policy.ModePlan {
			return // the person moved on during the turn; their choice stands
		}
		if was == policy.ModeBypass {
			e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "mode stays plan: bypass is chosen only at startup"})
			return
		}
		if err := e.modes.Set(ctx, was, agent.ViaSlash); err != nil {
			e.ui.Append(ui.Block{Kind: ui.BlockError, Text: "mode stays plan: " + err.Error()})
			return
		}
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "mode: " + string(was)})
	}, nil
}
