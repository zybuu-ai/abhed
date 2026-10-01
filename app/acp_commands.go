package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/hawkeye"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/customcmd"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// Slash commands (docs/architecture/studio-acp-contract.md §3.3): the built-in
// ones, the person's and the organisation's custom commands, the workspace's
// only when it is trusted, and skills. Built-in names win every other source.

// acpBuiltin is a built-in command as a prompt runs it.
type acpBuiltin struct {
	name, description, hint string
	run                     func(c *acpConn, ctx context.Context, s *acpSession, args string) (string, error)
}

var acpBuiltins = []acpBuiltin{
	{"/compact", "Summarise the conversation so far to free context", "", runCompact},
	{"/fork", "Go on in a new session from here (Studio's Fork action)", "", studioAction("Fork")},
	{"/undo", "Put back the files the last turn with changes changed", "", runUndo},
	{"/diff", "List the files this session changed", "", runDiff},
	{"/hawkeye", "Summarise this session's record", "", runHawkeye},
	{"/tasks", "List the background tasks", "", runTasks},
	{"/memory", "List the memory files in effect", "", runMemory},
	{"/mode", "Change the permission mode", "default | accept-edits | plan", runMode},
	{"/model", "Change the model", "a configured model's name", runModel},
	{"/clear", "Start a new conversation (Studio's New Chat action)", "", studioAction("New Chat")},
	{"/export", "Export this session's record to ~/.abhed/exports", "jsonl | html", runExport},
}

// sessionCommand is one command a session lists, with what it runs.
type sessionCommand struct {
	name, description, hint string
	source, sha256          string
	builtin                 *acpBuiltin
	custom                  *customcmd.Command
	skill                   string
}

// commandsFor lists a session's commands, built-ins first.
func commandsFor(s *acpSession) []sessionCommand {
	var out []sessionCommand
	taken := map[string]bool{}
	for i := range acpBuiltins {
		b := &acpBuiltins[i]
		out = append(out, sessionCommand{name: b.name, description: b.description, hint: b.hint, source: "builtin", builtin: b})
		taken[b.name] = true
	}
	if !s.inner {
		return out
	}
	// A workspace's commands are instructions: listed only when trusted, which
	// readCustomCommands decides from the file's content.
	cs := readCustomCommands(s.parts.Config, s.cwd)
	for _, c := range cs.cmds {
		if taken[c.Name] {
			continue
		}
		taken[c.Name] = true
		out = append(out, sessionCommand{name: c.Name, description: ui.VisibleLine(c.Description), hint: ui.VisibleLine(c.ArgumentHint),
			source: c.Source, sha256: c.SHA256, custom: c})
	}
	if set := s.parts.Set; set != nil && set.Skills != nil {
		names := set.Skills.Names()
		sort.Strings(names)
		for _, n := range names {
			sk, _ := set.Skills.Get(n)
			name := "/" + n
			if taken[name] || sk == nil {
				continue
			}
			taken[name] = true
			out = append(out, sessionCommand{name: name, description: ui.VisibleLine(firstSentence(sk.Description)), source: "skill", skill: n})
		}
	}
	return out
}

// sendCommands sends available_commands_update for a session.
func (c *acpConn) sendCommands(s *acpSession) {
	list := []any{}
	for _, cmd := range commandsFor(s) {
		meta := map[string]any{"source": cmd.source}
		if cmd.sha256 != "" {
			meta["sha256"] = cmd.sha256
		}
		entry := map[string]any{"name": strings.TrimPrefix(cmd.name, "/"), "description": cmd.description,
			"_meta": map[string]any{acpMetaKey: meta}}
		if cmd.hint != "" {
			entry["input"] = map[string]any{"hint": cmd.hint}
		}
		list = append(list, entry)
	}
	c.sessionUpdate(s.id, map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": list})
}

// slashCommand runs a prompt that names a command. handled is false for any
// other prompt, which goes to the agent as it is.
func (c *acpConn) slashCommand(ctx context.Context, s *acpSession, text string) (err error, handled bool) {
	if !strings.HasPrefix(text, "/") {
		return nil, false
	}
	name, args, _ := strings.Cut(text, " ")
	if i := strings.IndexAny(name, "\n\t"); i >= 0 {
		name, args = name[:i], name[i+1:]+" "+args
	}
	args = strings.TrimSpace(args)
	var cmd *sessionCommand
	for _, sc := range commandsFor(s) {
		if sc.name == name {
			cmd = &sc
			break
		}
	}
	if cmd == nil {
		return nil, false
	}
	say := func(t string) {
		c.sessionUpdate(s.id, map[string]any{"sessionUpdate": "agent_message_chunk", "content": textBlock(t)})
	}
	invoked := agent.CommandInvoked{Name: cmd.name, Source: cmd.source, SHA256: cmd.sha256, Args: args}
	switch {
	case cmd.builtin != nil:
		s.record(agent.EvCommandInvoked, invoked)
		// A built-in's refusal is its answer to the person, not the turn's failure.
		out, runErr := cmd.builtin.run(c, ctx, s, args)
		if runErr != nil {
			out = cmd.name + ": " + runErr.Error() + "\n"
		}
		say(out)
		return nil, true
	case cmd.custom != nil:
		// The CLI runs a command's shell lines and narrows its tools; over ACP
		// such a command is refused rather than run without them.
		if len(customcmd.InlineCommands(cmd.custom.Body)) > 0 || len(cmd.custom.AllowedTools) > 0 || cmd.custom.Model != "" {
			say(cmd.name + " runs shell lines, narrows the tools or picks a model, which this engine does only in the CLI.\n")
			return nil, true
		}
		s.record(agent.EvCommandInvoked, invoked)
		_, err := s.agent.Run(ctx, customcmd.Expand(cmd.custom.Body, args))
		return err, true
	default:
		s.record(agent.EvCommandInvoked, invoked)
		prompt := fmt.Sprintf("Use the skill %q.", cmd.skill)
		if args != "" {
			prompt += " " + args
		}
		_, err := s.agent.Run(ctx, prompt)
		return err, true
	}
}

func studioAction(action string) func(*acpConn, context.Context, *acpSession, string) (string, error) {
	return func(*acpConn, context.Context, *acpSession, string) (string, error) {
		return "", fmt.Errorf("use Studio's %s action", action)
	}
}

func runCompact(_ *acpConn, ctx context.Context, s *acpSession, _ string) (string, error) {
	parts, e := innerOf(s)
	if e != nil {
		return "", errors.New(e.Message)
	}
	s.record(agent.EvCompactStarted, map[string]any{"trigger": "manual", "by": agent.ByUser})
	info, err := parts.Loop.Compact(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Compacted: %d tokens to %d.\n", info.BeforeTokens, info.AfterTokens), nil
}

func runUndo(c *acpConn, _ context.Context, s *acpSession, _ string) (string, error) {
	if !s.inner {
		return "", errors.New("this session cannot undo")
	}
	cps := s.undo.Checkpoints()
	if len(cps) == 0 {
		return "Nothing to undo.\n", nil
	}
	last := cps[len(cps)-1].Turn
	paths, err := c.restoreFrom(s, last)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Put back %d file(s): %s\n", len(paths), strings.Join(paths, ", ")), nil
}

func runDiff(_ *acpConn, _ context.Context, s *acpSession, _ string) (string, error) {
	if !s.inner {
		return "", errors.New("this session has no changes to show")
	}
	var b strings.Builder
	for _, path := range s.undo.Changed() {
		if f, ok := s.reviewOf(path); ok && len(f.hunks) > 0 {
			fmt.Fprintf(&b, "%s  %s, %d hunk(s)\n", f.status(), path, len(f.hunks))
		}
	}
	if b.Len() == 0 {
		return "No changes.\n", nil
	}
	return b.String(), nil
}

func runHawkeye(c *acpConn, _ context.Context, s *acpSession, _ string) (string, error) {
	rec, e, rerr := c.recorded(s.id)
	if rerr != nil {
		return "", errors.New("HawkEYE reads the durable record, which this session is not in")
	}
	return hawkeye.Text(hawkeyeOf(rec, e)), nil
}

func runTasks(_ *acpConn, _ context.Context, s *acpSession, _ string) (string, error) {
	t, ok := s.agent.(tasker)
	if !ok || len(t.Background()) == 0 {
		return "No background tasks.\n", nil
	}
	var b strings.Builder
	for _, ti := range t.Background() {
		fmt.Fprintf(&b, "%s  %s  %s\n", ti.ID, ti.Status, ui.VisibleLine(ti.Description))
	}
	return b.String(), nil
}

func runMemory(_ *acpConn, _ context.Context, s *acpSession, _ string) (string, error) {
	if !s.inner {
		return "", errors.New("this session has no memory files")
	}
	files := agent.LoadMemory(memoryOptions(s.parts.Config, s.parts.Loop.Policy, s.cwd)).Files()
	if len(files) == 0 {
		return "No memory files.\n", nil
	}
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "%s  (%s)\n", f.Path, f.Scope)
	}
	return b.String(), nil
}

// runMode changes the mode from a prompt. Auto and bypass are not reached
// this way: Studio's mode picker asks first, and a typed line does not.
func runMode(c *acpConn, _ context.Context, s *acpSession, args string) (string, error) {
	mode := policy.Mode(strings.TrimSpace(args))
	if mode == policy.ModeAuto || mode == policy.ModeBypass {
		return "", fmt.Errorf("choose %s from the mode picker, which asks first", mode)
	}
	if mode == "" {
		if !s.inner {
			return "", errors.New("this session has no mode")
		}
		return "Mode: " + string(s.parts.Loop.Policy.Mode) + "\n", nil
	}
	// Run from inside the prompt's own turn, so the busy check does not apply.
	parts, e := innerOf(s)
	if e != nil {
		return "", errors.New(e.Message)
	}
	if s.liveTasks() > 0 {
		return "", errors.New("background tasks are running; change the mode once they end")
	}
	if !contains(availableModes(parts.Config), mode) {
		return "", fmt.Errorf("the mode %q is not available in this session", ui.VisibleLine(string(mode)))
	}
	from := parts.Loop.Policy.Mode
	if from != mode {
		parts.Loop.Policy.Mode = mode
		s.record(agent.EvModeChanged, agent.ModeChanged{From: string(from), To: string(mode), By: agent.ByUser, Via: agent.ViaStudio})
		c.sessionUpdate(s.id, map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": string(mode)})
	}
	return "Mode: " + string(mode) + "\n", nil
}

func contains(modes []policy.Mode, m policy.Mode) bool {
	for _, x := range modes {
		if x == m {
			return true
		}
	}
	return false
}

func runModel(c *acpConn, _ context.Context, s *acpSession, args string) (string, error) {
	m, ok := s.agent.(modelSwitcher)
	if !ok {
		return "", errors.New("this session's model cannot be changed")
	}
	name := strings.TrimSpace(args)
	if name == "" {
		return "Model: " + currentModel(m.Models()) + "\n", nil
	}
	if err := m.SwitchModelNamed(name); err != nil {
		return "", fmt.Errorf("no model named %q is configured for this session", ui.VisibleLine(name))
	}
	return "Model: " + name + "\n", nil
}

func runExport(c *acpConn, _ context.Context, s *acpSession, args string) (string, error) {
	format := strings.TrimSpace(args)
	if format == "" {
		format = "jsonl"
	}
	if format != "jsonl" && format != "html" {
		return "", errors.New(`the format is "jsonl" or "html"`)
	}
	rec, e, rerr := c.recorded(s.id)
	if rerr != nil {
		return "", errors.New("an export reads the durable record, which this session is not in")
	}
	path, err := exportPath("", e.ID, format)
	if err != nil {
		return "", err
	}
	if format == "jsonl" {
		_, err = exportSession(rec, e, "jsonl", path, io.Discard, false)
	} else {
		err = writeHawkeyeExport(rec, e, path)
	}
	if err != nil {
		return "", err
	}
	return "Wrote " + path + "\n", nil
}
