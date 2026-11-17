package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/customcmd"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/commands", Args: "[show <name>|trust]", Help: "list custom commands, show one, or review the workspace's",
		Group: "context", Order: 95, Run: slashCommands})
}

// customState is the session's custom commands.
type customState struct {
	cmds []*customcmd.Command
	// untrusted are the workspace's commands awaiting trust: listed, never run.
	untrusted []*customcmd.Command
	wsFiles   []customcmd.File
	wsSum     string
	wsReason  string // stored, flag, env, new, changed, declined or refused
	problems  []string
}

// customSource hands one kind of custom command to the registry, which
// stamps every command with that kind.
type customSource struct {
	kind string
	cmds []slashCmd
}

func (c customSource) Source() string            { return c.kind }
func (c customSource) SlashCommands() []slashCmd { return c.cmds }

// commandTrustStore is where workspace command decisions are kept; a
// variable for tests.
var commandTrustStore = customcmd.DefaultTrustStore

// ensureCustomCommands loads the session's custom commands once.
func ensureCustomCommands(st *cliState, r *ui.Renderer) {
	if st.input.custom != nil {
		return
	}
	loadCustomCommands(st)
	sf := surfaceOf(st, r)
	for _, p := range st.input.custom.problems {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: p})
	}
	for _, w := range slashWarnings(st.dynamic) {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: w})
	}
	if n := len(st.input.custom.untrusted); n > 0 {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: fmt.Sprintf(
			"%d workspace command(s) in %s are not trusted (%s), so they do not run; /commands trust, or `abhed trust` at a shell, reviews them",
			n, customcmd.WorkspaceDir, st.input.custom.wsReason)})
	}
}

// loadCustomCommands reads the managed, user and workspace commands and
// registers them, then the MCP servers' prompts, as run-time sources:
// built-ins still win every name.
func loadCustomCommands(st *cliState) {
	ws := st.workspace
	if ws == "" && st.sess != nil {
		ws = st.sess.Root
	}
	cs := readCustomCommands(st.appCfg, ws)
	st.input.custom = cs
	cmds := cs.cmds

	bySource := map[string][]slashCmd{}
	for _, c := range cmds {
		bySource[c.Source] = append(bySource[c.Source], customSlash(c, true))
	}
	for _, c := range cs.untrusted {
		bySource[customcmd.SourceWorkspace] = append(bySource[customcmd.SourceWorkspace], customSlash(c, false))
	}
	// Earlier sources win a name the registry has not given a built-in.
	st.dynamic = nil
	for _, kind := range []string{customcmd.SourceManaged, customcmd.SourceUser, customcmd.SourceWorkspace} {
		if len(bySource[kind]) > 0 {
			st.dynamic = append(st.dynamic, customSource{kind: kind, cmds: bySource[kind]})
		}
	}
	// MCP prompts come last: a custom command or built-in keeps its name.
	st.dynamic = append(st.dynamic, mcpPromptSource{st: st})
	ui.SetCommands(builtinSlash.uiCommands(st.dynamic))
}

// decideCommands is whether the workspace's commands with this hash run:
// the choice made when the session started (--trust, the environment,
// refusal), then the person's stored decision for exactly this content.
func decideCommands(w config.WorkspaceTrust, ws, sum string) (bool, string) {
	switch w.Reason {
	case "refused":
		return false, "refused"
	case "flag", "env":
		return true, w.Reason
	}
	if v := os.Getenv(config.TrustEnv); v == "1" || strings.EqualFold(v, "true") {
		return true, "env"
	}
	store, err := commandTrustStore()
	if err != nil {
		return false, "new"
	}
	return store.Decide(ws, sum)
}

func isHomeDir(ws, home string) bool {
	if home == "" {
		return false
	}
	a, _ := filepath.EvalSymlinks(ws)
	b, _ := filepath.EvalSymlinks(home)
	return a != "" && a == b
}

// customSlash is a custom command as the registry holds it. One not trusted
// is listed, and running it says how to review it.
func customSlash(c *customcmd.Command, runnable bool) slashCmd {
	help := c.Description
	if help == "" {
		help = "custom command"
	}
	help += " (" + c.Source + ")"
	run := func(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
		return false, runCustom(ctx, e, c, args)
	}
	if !runnable {
		help = "not trusted: /commands trust reviews it"
		run = func(context.Context, *cmdEnv, []string) (bool, error) {
			return false, fmt.Errorf("%s came with the workspace and is not trusted; /commands trust, or `abhed trust` at a shell, reviews it", c.Name)
		}
	}
	return slashCmd{Name: c.Name, Args: c.ArgumentHint, Help: help, Group: "custom", Run: run}
}

// runCustom runs a custom command: its arguments substituted, its inline
// shell lines run through policy (each one asks), its @ files attached through
// policy, recorded as command.invoked, and its text sent as the next turn,
// narrowed to its tools and on its model.
func runCustom(ctx context.Context, e *cmdEnv, c *customcmd.Command, args []string) error {
	st, sf := e.st, e.ui
	// Checked before the tools narrow or the model moves.
	if err := st.turnFree(); err != nil {
		return err
	}
	if err := ensureConversation(ctx, st); err != nil {
		return err
	}
	recordMemoryLoaded(st)
	loop := st.loop
	argText := strings.Join(args, " ")
	if _, err := loop.Recorder.Record(agent.EvCommandInvoked, agent.ActorUser, agent.Trusted, agent.CommandInvoked{
		Name: c.Name, Source: c.Source, SHA256: c.SHA256, Args: argText, // the recorder redacts
	}); err != nil {
		return err
	}
	body := customcmd.Expand(c.Body, argText)
	// The command's own @ files are attached first, with each shell line held
	// as a placeholder, so no output can name a file read as the person's.
	var inline []string
	ph := fenced("pending", "", "")
	body = customcmd.ReplaceInline(body, func(cmd string) string {
		inline = append(inline, cmd)
		return fmt.Sprintf("%s#%d#", ph, len(inline)-1)
	})
	msg, _, err := (mentionExpander{st: st}).Expand(ctx, loop, body)
	if err != nil {
		return fmt.Errorf("%s was not sent: %w", c.Name, err)
	}
	for i, cmd := range inline {
		res, ran := personBash(ctx, st, sf, cmd, "run by the custom command "+c.Name, c.Name)
		if !ran {
			return fmt.Errorf("%s was not sent: one of its shell lines did not run", c.Name)
		}
		out, _ := capText(strings.TrimRight(res.Content, "\n"), bangOutputMax)
		msg.Text = strings.Replace(msg.Text, fmt.Sprintf("%s#%d#", ph, i), fenced("command-output", "", out), 1)
	}

	var undo []func()
	if len(c.AllowedTools) > 0 {
		sub, missing := loop.Tools.SubsetStrict(c.AllowedTools)
		if len(missing) > 0 {
			return fmt.Errorf("%s names tools this session does not have: %s", c.Name, strings.Join(missing, ", "))
		}
		prev := loop.Tools
		loop.Tools = sub
		undo = append(undo, func() { loop.Tools = prev })
	}
	if c.Model != "" && c.Model != st.appCfg.Model.Default {
		was := st.appCfg.Model.Default
		if err := switchCommandModel(st, c.Model); err != nil {
			for _, u := range undo {
				u()
			}
			return fmt.Errorf("%s was not sent: %w", c.Name, err)
		}
		undo = append(undo, func() {
			// A /model run since, such as one queued during the turn, stands.
			if st.appCfg.Model.Default != c.Model {
				return
			}
			if err := switchCommandModel(st, was); err != nil {
				sf.Append(ui.Block{Kind: ui.BlockError, Text: "could not switch back to " + was + ": " + err.Error()})
			}
		})
	}
	restore := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	// The command's body is not the person's own words.
	msg.Typed = ""
	if err := st.sendTurn(msg, restore); err != nil {
		restore()
		return err
	}
	return nil
}

// switchCommandModel moves the conversation to a configured provider, recorded as
// /model records it.
func switchCommandModel(st *cliState, name string) error {
	if err := st.appCfg.ModelAllowed(name); err != nil {
		var refused *config.ManagedError
		if errors.As(err, &refused) {
			recordModelRefused(st, "custom command model", refused)
		}
		return err
	}
	p, err := st.appCfg.ProviderNamed(name)
	if err != nil {
		return err
	}
	next, err := newAdapter(p)
	if err != nil {
		return err
	}
	if st.loop != nil {
		if err := st.loop.SwitchModel(name, next); err != nil {
			return err
		}
	}
	st.appCfg.Model.Default, st.provider, st.adapter, st.moved = name, p, next, nil
	return nil
}

// slashCommands is /commands.
func slashCommands(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	st, sf := e.st, e.ui
	ensureCustomCommands(st, e.r)
	cs := st.input.custom
	switch {
	case len(args) == 1 && args[0] == "trust":
		return false, trustCommands(ctx, st, sf)
	case len(args) == 2 && args[0] == "show":
		for _, c := range append(append([]*customcmd.Command{}, cs.cmds...), cs.untrusted...) {
			if c.Name == args[1] || c.Name == "/"+args[1] {
				return false, sf.Panel(ctx, ui.PanelSpec{Title: c.Name + " (" + c.Source + ", " + config.Printable(c.Path) + ")",
					Body: []ui.Block{{Kind: ui.BlockMarkdown, Text: c.Body}}})
			}
		}
		return false, fmt.Errorf("no custom command %s", args[1])
	case len(args) > 0:
		return false, errors.New("usage: /commands [show <name>|trust]")
	}
	if len(cs.cmds)+len(cs.untrusted) == 0 {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "no custom commands; a markdown file in ~/.abhed/commands/ is one, named by the file"})
		return false, nil
	}
	rows := [][]string{{"command", "source", "sha256", "description"}}
	for _, c := range cs.cmds {
		rows = append(rows, []string{c.Name, c.Source, shortSum(c.SHA256), c.Description})
	}
	for _, c := range cs.untrusted {
		rows = append(rows, []string{c.Name, "workspace, not trusted", shortSum(c.SHA256), "review with /commands trust"})
	}
	sf.Append(ui.Block{Kind: ui.BlockTable, Rows: rows})
	return false, nil
}

// trustCommands shows the workspace's commands and asks whether to trust
// exactly this content. The answer is kept, and the commands reload.
func trustCommands(ctx context.Context, st *cliState, sf ui.Surface) error {
	cs := st.input.custom
	if len(cs.wsFiles) == 0 {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "this workspace has no commands in " + customcmd.WorkspaceDir})
		return nil
	}
	if len(cs.untrusted) == 0 && cs.wsReason != "declined" && cs.wsReason != "new" && cs.wsReason != "changed" {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "the workspace's commands are trusted (" + cs.wsReason + ")"})
		return nil
	}
	if cs.wsReason == "refused" {
		return errors.New("the workspace was started untrusted; its commands stay off for this session")
	}
	rows := [][]string{{"file", "sha256", "first line"}}
	for _, f := range cs.wsFiles {
		first := commandSummary(f)
		rows = append(rows, []string{f.Key, shortSum(config.HashOf(f.Data)), first})
	}
	choice, err := sf.Dialog(ctx, ui.DialogSpec{
		Kind:  ui.DialogConfirm,
		Title: "Trust these workspace commands?",
		Body: []ui.Block{{Kind: ui.BlockTable, Rows: rows},
			{Kind: ui.BlockNotice, Text: "A command is instructions to the agent, and can run shell lines (each asks). " +
				"/commands show <name> prints one. The trust covers exactly this content (sha256 " + shortSum(cs.wsSum) + ")."}},
		Why: "workspace commands · " + cs.wsReason,
	})
	if err != nil {
		if !errors.Is(err, ui.ErrNoAnswer) {
			return err
		}
		// No answer decides nothing: it is asked again next time.
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "not trusted: no answer"})
		return nil
	}
	trusted := choice == ui.ChoiceYes
	store, serr := commandTrustStore()
	if serr != nil {
		return serr
	}
	if err := store.Record(st.sess.Root, cs.wsSum, trusted); err != nil {
		return err
	}
	st.input.custom = nil
	loadCustomCommands(st)
	if trusted {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: fmt.Sprintf("trusted %d workspace command(s)", len(cs.wsFiles))})
	} else {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "not trusted; they stay off until they are trusted"})
	}
	return nil
}

// readCustomCommands reads the managed, user and workspace commands for a
// workspace; the workspace's run only under a trust decision for their content.
func readCustomCommands(cfg config.Config, ws string) *customState {
	cs := &customState{}
	var models []string
	for name := range cfg.Model.Providers {
		models = append(models, name)
	}
	sort.Strings(models)
	userDirs, files, sum, problems := workspaceCommands(cfg, ws)
	cs.problems = problems
	cs.wsFiles, cs.wsSum = files, sum

	var trusted []customcmd.File
	if len(files) > 0 {
		var ok bool
		ok, cs.wsReason = decideCommands(cfg.Workspace, ws, sum)
		if ok {
			trusted = files
		} else {
			for _, f := range files {
				if c, err := customcmd.Parse(f, customcmd.SourceWorkspace, models); err == nil {
					cs.untrusted = append(cs.untrusted, c)
				}
			}
		}
	}
	cmds, errs := customcmd.Load(customcmd.Options{ManagedDir: managed.CommandsDir, UserDirs: userDirs, Workspace: trusted, Models: models})
	for _, e := range errs {
		cs.problems = append(cs.problems, e.Error())
	}
	cs.cmds = cmds
	return cs
}

// workspaceCommands splits the command directories into the person's own
// and the workspace's, and reads the workspace's files with the hash a trust
// decision covers. It is what a session and `abhed trust` both read.
func workspaceCommands(cfg config.Config, ws string) (userDirs []string, files []customcmd.File, sum string, problems []string) {
	home, _ := os.UserHomeDir()
	// The workspace is the home directory or holds it: every path under home
	// is in the workspace, so only a relative entry is taken as the workspace's.
	atHome := home != "" && ws != "" && customcmd.Inside(ws, home)
	var relative []string
	if home != "" {
		userDirs = append(userDirs, filepath.Join(home, ".abhed", "commands"))
	}
	for _, d := range cfg.Commands.Dirs {
		switch {
		case strings.HasPrefix(d, "~/") && home != "":
			d = filepath.Join(home, d[2:])
		case !filepath.IsAbs(d):
			relative = append(relative, filepath.Join(ws, d))
			continue
		}
		userDirs = append(userDirs, d)
	}
	if ws == "" {
		return userDirs, nil, "", nil
	}
	// A configured directory inside the workspace came with it, whoever
	// named it: its commands need the same trust as .abhed/commands.
	inside := relative
	var outside []string
	for _, d := range userDirs {
		if !atHome && customcmd.Inside(ws, d) {
			inside = append(inside, d)
		} else {
			outside = append(outside, d)
		}
	}
	var errs []error
	if !isHomeDir(ws, home) { // at home, .abhed/commands is the person's own
		files, _, errs = customcmd.ReadWorkspace(ws)
	}
	more, moreErrs := customcmd.ReadWorkspaceDirs(ws, inside)
	files = append(files, more...)
	errs = append(errs, moreErrs...)
	if len(files) > 0 {
		sum = customcmd.HashFiles(files)
	}
	for _, e := range errs {
		problems = append(problems, e.Error())
	}
	return outside, files, sum, problems
}

// headlessCustom is the custom command a -p task names, with its arguments.
// A workspace command not trusted is refused with the command that trusts
// it; a task that names no custom command is sent as it is.
func headlessCustom(cs *customState, ws, task string) (*customcmd.Command, string, error) {
	if !strings.HasPrefix(task, "/") || cs == nil {
		return nil, "", nil
	}
	name, args, _ := strings.Cut(task, " ")
	if i := strings.IndexAny(name, "\n\t"); i >= 0 {
		name, args = name[:i], name[i+1:]+" "+args
	}
	args = strings.TrimSpace(args)
	for _, c := range cs.cmds {
		if c.Name == name {
			// A -p run cannot ask about a shell line, and its model is the run's.
			switch {
			case len(customcmd.InlineCommands(c.Body)) > 0:
				return nil, "", fmt.Errorf("%s runs shell lines, which ask first; -p cannot ask, so run it in the terminal", c.Name)
			case c.Model != "":
				return nil, "", fmt.Errorf("%s picks the model %s; in -p choose it with -model and give the command's text as the task", c.Name, c.Model)
			}
			return c, args, nil
		}
	}
	for _, c := range cs.untrusted {
		if c.Name != name {
			continue
		}
		if cs.wsReason == "refused" {
			return nil, "", fmt.Errorf("workspace command %s was not run: the workspace was started untrusted, so its commands stay off", c.Name)
		}
		return nil, "", fmt.Errorf("workspace command %s is not trusted (%s), so it was not run; review the workspace's commands with `abhed trust %s`, "+
			"then trust exactly those commands, and nothing else of the workspace, with: abhed trust grant -commands-sha256 %s %s",
			c.Name, cs.wsReason, shellQuote(ws), cs.wsSum, shellQuote(ws))
	}
	return nil, "", nil
}

// commandSummary is what a review shows of a command file: its description,
// or else the first line of its body, never the front matter's "---".
func commandSummary(f customcmd.File) string {
	if c, err := customcmd.Parse(f, customcmd.SourceWorkspace, nil); err == nil {
		if d := strings.TrimSpace(c.Description); d != "" {
			return d
		}
		for _, line := range strings.Split(c.Body, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
		return ""
	}
	// A file that does not parse is shown by its first line past any front
	// matter, or as it begins.
	lines := strings.Split(string(f.Data), "\n")
	start := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				start = i + 1
				break
			}
		}
	}
	for _, line := range lines[start:] {
		if line = strings.TrimSpace(line); line != "" && line != "---" {
			return line
		}
	}
	return ""
}
