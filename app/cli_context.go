package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/compact", Args: "[focus]", Help: "compact the context now, keeping what focus names", Group: "context", Order: 50, Run: legacy("/compact", slashCompact)})
	registerSlash(slashCmd{Name: "/context", Help: "what fills the context window, in tokens and percent", Group: "context", Order: 45, ReadOnly: true, Run: slashContext})
	registerSlash(slashCmd{Name: "/init", Args: "[notes]", Help: "have the agent write ABHED.md from the repository", Group: "context", Order: 85, Run: slashInit})
	registerSlash(slashCmd{Name: "/memory", Args: "[show <n>|add <scope> <note>|auto on|off]", Help: "show the ABHED.md files in effect", Group: "context", Order: 90, Run: slashMemory})
}

// slashCompact is /compact.
func slashCompact(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	if st.loop == nil {
		fmt.Println(s.Dim("  nothing to compact yet"))
		return false
	}
	// It writes to the session's record, so the session is claimed first.
	release, err := claimForWrite(ctx, st)
	if err != nil {
		fmt.Printf("  %s not continued: %v\n", s.Red("✕"), err)
		return false
	}
	// The focus is taken by this compaction's summary, or dropped with it.
	if st.loop.Compactor != nil {
		st.loop.Compactor.SetFocus(strings.Join(fields[1:], " "))
	}
	info, err := st.loop.Compact(ctx)
	if st.loop.Compactor != nil {
		st.loop.Compactor.SetFocus("")
	}
	release()
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	fmt.Printf("  compacted %d → %d tokens\n", info.BeforeTokens, info.AfterTokens)
	return false
}

// sessionMemory is the memory the session's system prompt was built with.
// The prompt is built once per process, so the memory is too; memory.loaded
// records it in each conversation.
var sessionMemory atomic.Pointer[agent.Memory]

// memoryOptions are where the CLI's memory comes from: the configured import
// depth and rule directories, and the session's read rules for workspace
// files, which a deny keeps out of the prompt as it keeps them from a read.
func memoryOptions(cfg config.Config, pol *policy.Engine, workspace string) agent.MemoryOptions {
	o := agent.MemoryOptions{Workspace: workspace, ImportDepth: cfg.MemoryImportDepth(), RuleDirs: cfg.Rules.Dirs}
	if home, err := os.UserHomeDir(); err == nil && cfg.Memory.Auto {
		o.Auto = tools.AutoMemoryPath(home, workspace)
	}
	if pol != nil {
		o.Allow = func(path string) error {
			if d := pol.Evaluate("read", false, argsJSON(map[string]string{"path": path})); d.Decision == policy.Deny {
				return errors.New(d.Reason)
			}
			return nil
		}
	}
	return o
}

// cliSystemPrompt is the main agent's prompt, as toolset.SystemPrompt builds
// it, with the memory the configuration describes.
func cliSystemPrompt(cfg config.Config, pol *policy.Engine, workspace string, adapter model.Adapter, skills string, toolNames []string) string {
	mem := agent.LoadMemory(memoryOptions(cfg, pol, workspace))
	sessionMemory.Store(mem)
	return agent.BuildSystemPrompt(agent.BuildOptions{
		Profile:       "main",
		Workspace:     workspace,
		Model:         adapter.Profile().Name,
		ContextWindow: adapter.Profile().ContextWindow,
		Memory:        mem,
		Skills:        skills,
		Tools:         toolNames,
	})
}

// recordMemoryLoaded records, once in each conversation, which memory files
// its prompt carries and their hashes.
func recordMemoryLoaded(st *cliState) {
	mem := sessionMemory.Load()
	if st.loop == nil || mem == nil || st.input.memoryFor == st.loop {
		return
	}
	files := mem.Files()
	if len(files) == 0 {
		st.input.memoryFor = st.loop
		return
	}
	if _, err := st.loop.Recorder.Record(agent.EvMemoryLoaded, agent.ActorSystem, agent.Trusted, agent.MemoryLoaded{Files: files}); err == nil {
		st.input.memoryFor = st.loop
	}
}

// slashMemory is /memory: the files in effect, one shown whole, or a note
// added to one of them.
func slashMemory(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	sf := e.ui
	mem := sessionMemory.Load()
	if mem == nil {
		mem = agent.LoadMemory(memoryOptions(e.st.appCfg, e.pol, e.sess.Root))
	}
	switch {
	case len(args) >= 1 && args[0] == "auto":
		return false, memoryAuto(e, args[1:])
	case len(args) >= 1 && args[0] == "add":
		if len(args) < 3 {
			return false, errors.New("usage: /memory add <project|local|user> <note>")
		}
		saveNote(ctx, e.st, sf, args[1], strings.Join(args[2:], " "))
		return false, nil
	case len(args) == 2 && args[0] == "show":
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 || n > len(mem.Entries) {
			return false, fmt.Errorf("no memory file %s; /memory lists them", args[1])
		}
		ent := mem.Entries[n-1]
		if ent.Skipped != "" {
			return false, fmt.Errorf("%s was not loaded: %s", ent.Label, ent.Skipped)
		}
		return false, sf.Panel(ctx, ui.PanelSpec{Title: ent.Label, Body: []ui.Block{{Kind: ui.BlockMarkdown, Text: ent.Content}}})
	case len(args) > 0:
		return false, errors.New("usage: /memory [show <n>|add <project|local|user> <note>]")
	}
	if len(mem.Entries) == 0 {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "no memory file yet; # <note> or /memory add project <note> starts ABHED.md.\n" +
			"memory is put into every request, so keep it short"})
		return false, nil
	}
	rows := [][]string{{"#", "scope", "file", "size", "sha256"}}
	for i, ent := range mem.Entries {
		size, sum := fmt.Sprintf("%d B", len(ent.Content)), shortSum(ent.SHA256)
		if ent.Skipped != "" {
			size, sum = "not loaded", ent.Skipped
		}
		label := ent.Label
		if len(ent.Paths) > 0 {
			label += " (for " + strings.Join(ent.Paths, ", ") + ")"
		}
		if ent.From != "" {
			label += " (imported by " + filepath.Base(ent.From) + ")"
		}
		rows = append(rows, []string{strconv.Itoa(i + 1), ent.Scope, label, size, sum})
	}
	sf.Append(ui.Block{Kind: ui.BlockTable, Rows: rows})
	sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "/memory show <n> prints one; # <note> adds to one. " +
		"Project memory came with the workspace: it is instructions, read only as its files allow."})
	return false, nil
}

func shortSum(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Memory targets a note can go to.
const (
	noteProject = "project" // the workspace's ABHED.md, shared with the team
	noteLocal   = "local"   // the workspace's ABHED.local.md, the person's own
	noteUser    = "user"    // ~/.abhed/ABHED.md, every workspace
)

// addNote is # at the prompt: the person picks which memory file the note
// goes to, and it is appended there.
func addNote(ctx context.Context, st *cliState, r *ui.Renderer, note string) {
	sf := surfaceOf(st, r)
	if note == "" {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "# saves a note to memory: # always run go vet before committing"})
		return
	}
	target, err := sf.Dialog(ctx, ui.DialogSpec{
		Kind:  ui.DialogChoice,
		Title: "Save this note to which memory?",
		Body:  []ui.Block{{Kind: ui.BlockNotice, Text: note}},
		Choices: []ui.Choice{
			{ID: noteProject, Label: "Project memory (ABHED.md, shared with the repository)", Key: 'p'},
			{ID: noteLocal, Label: "Local project memory (ABHED.local.md, yours)", Key: 'l'},
			{ID: noteUser, Label: "User memory (~/.abhed/ABHED.md, every workspace)", Key: 'u'},
		},
		Default: noteProject,
	})
	if err != nil {
		sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "not saved: no memory was chosen"})
		return
	}
	saveNote(ctx, st, sf, target, note)
}

// saveNote appends note to the target memory file and records
// memory.written. A workspace file is written as the person's write call, so
// the policy decides (a deny rule, plan mode) and the boundary holds; the
// user's own file is theirs and is written as it is, never through a link.
// The note is redacted before it is written.
func saveNote(ctx context.Context, st *cliState, sf ui.Surface, target, note string) {
	if err := ensureConversation(ctx, st); err != nil {
		sf.Append(ui.Block{Kind: ui.BlockError, Text: "not saved: " + err.Error()})
		return
	}
	loop := st.loop
	note = redactFor(loop, strings.TrimSpace(note))
	var shown string
	var err error
	switch target {
	case noteProject, noteLocal:
		name := "ABHED.md"
		if target == noteLocal {
			name = "ABHED.local.md"
		}
		shown, err = appendWorkspaceNote(ctx, loop, st.sess, filepath.Join(st.sess.Root, name), note)
	case noteUser:
		shown, err = appendUserNote(note)
	default:
		err = fmt.Errorf("no memory %q; use project, local or user", target)
	}
	if err != nil {
		sf.Append(ui.Block{Kind: ui.BlockError, Text: "not saved: " + err.Error()})
		return
	}
	if _, err := loop.Recorder.Record(agent.EvMemoryWritten, agent.ActorUser, agent.Trusted,
		agent.MemoryWritten{Path: shown, Kind: "note", By: "user"}); err != nil {
		sf.Append(ui.Block{Kind: ui.BlockError, Text: "saved, but not recorded: " + err.Error()})
		return
	}
	sf.Append(ui.Block{Kind: ui.BlockNotice, Text: "saved to " + shown + "; it is read at the start of each session"})
}

// appendWorkspaceNote appends a note to a memory file in the workspace as
// the person's write, and returns its path relative to the workspace.
func appendWorkspaceNote(ctx context.Context, loop *agent.Loop, sess *tools.Session, abs, note string) (string, error) {
	// The confined read refuses a link out of the workspace or into state.
	existed := true
	before, err := sess.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		existed, err = false, nil
	}
	if err != nil {
		return "", err
	}
	after := appendItem(before, note)
	id := personCallID("note")
	args := argsJSON(map[string]string{"path": abs, "content": string(after)})
	_, refused, err := loop.ManualAuthorize("write", id, args)
	if err != nil {
		return "", err
	}
	if refused != nil {
		return "", errors.New(strings.TrimSpace(refused.Content))
	}
	if sess.Checkpoint != nil {
		sess.Checkpoint(abs, before, existed)
	}
	start := time.Now()
	werr := sess.RestoreFile(abs, after)
	res := tools.Result{Content: "appended a note to " + sess.Rel(abs)}
	if werr != nil {
		res = tools.Result{Content: werr.Error(), IsError: true}
	}
	if err := loop.ManualObserve(id, "write", res, time.Since(start)); err != nil {
		return "", err
	}
	if werr != nil {
		return "", werr
	}
	return filepath.ToSlash(sess.Rel(abs)), nil
}

// appendUserNote appends a note to ~/.abhed/ABHED.md.
func appendUserNote(note string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".abhed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "ABHED.md")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file; a link is not written through", path)
	}
	before, err := os.ReadFile(path) // #nosec G304 -- the person's own memory file under their home
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := writeFileAtomic(path, appendItem(before, note), 0o600); err != nil {
		return "", err
	}
	return "~/.abhed/ABHED.md", nil
}

// appendItem adds note to content as a list item on a line of its own.
func appendItem(content []byte, note string) []byte {
	out := append([]byte(nil), content...)
	if len(out) > 0 && !strings.HasSuffix(string(out), "\n") {
		out = append(out, '\n')
	}
	note = strings.ReplaceAll(note, "\n", " ")
	return append(out, []byte("- "+note+"\n")...)
}

// writeFileAtomic writes data to path through a temporary file beside it.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".abhed-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// initPrompt is what /init asks the agent to do. It names no other product
// or its files: ABHED.md is written from what this repository shows.
const initPrompt = `Write ABHED.md at the root of this workspace: the project memory Abhed reads at the start of every session. Base it on what the repository itself shows; do not guess.

First read the README, the build and package files, and the CI configuration. Then write ABHED.md, short (under about 150 lines) and specific to this repository:

1. What the project is, in two or three sentences.
2. How to build, test, lint and run it: the exact commands, including how to run a single test.
3. The layout: the main directories and what each holds, only where a newcomer would not guess it.
4. Conventions a newcomer would get wrong: style, naming, error handling, and the commit and review rules the repository states.

If ABHED.md already exists, improve it: keep what is still true, fix what is not, and remove what the code no longer supports. Do not copy text from configuration files written for other tools; state only what this repository's code and documents show.`

// slashInit is /init: the agent studies the repository and writes
// ABHED.md, through the write tool, so the change is shown and approved as
// any other write is. The record shows /init, as command.invoked.
func slashInit(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	st := e.st
	if err := ensureConversation(ctx, st); err != nil {
		return false, err
	}
	recordMemoryLoaded(st)
	if _, err := st.loop.Recorder.Record(agent.EvCommandInvoked, agent.ActorUser, agent.Trusted,
		agent.CommandInvoked{Name: "/init", Source: sourceBuiltin, Args: strings.Join(args, " ")}); err != nil {
		return false, err
	}
	text := initPrompt
	if extra := strings.TrimSpace(strings.Join(args, " ")); extra != "" {
		text += "\n\nThe person adds: " + extra
	}
	st.sendTurn(agent.Message{Text: text}, nil)
	return false, nil
}

// contextPart is one share of the context window.
type contextPart struct {
	label  string
	tokens int
}

// contextBreakdown measures what fills the conversation's context with the
// model's own counter: the system prompt apart from memory, memory, the
// built-in tools' definitions, MCP tools' definitions, and the messages.
func contextBreakdown(st *cliState) ([]contextPart, int) {
	a := st.adapter
	if a == nil && st.loop != nil {
		a = st.loop.Adapter
	}
	if a == nil {
		return nil, 0
	}
	count := func(req model.Request) int {
		n, err := a.CountTokens(req)
		if err != nil {
			return 0
		}
		return n
	}
	var memory int
	if mem := sessionMemory.Load(); mem != nil {
		if text := mem.Render(); text != "" {
			memory = count(model.Request{System: text})
		}
	}
	parts := []contextPart{{"memory files", memory}}
	if st.loop != nil {
		system := max(0, count(model.Request{System: st.loop.Config.SystemPrompt})-memory)
		var builtin, mcp []model.ToolDef
		if st.loop.Tools != nil {
			for _, d := range st.loop.Tools.Definitions() {
				td := model.ToolDef{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
				if strings.HasPrefix(d.Name, "mcp__") {
					mcp = append(mcp, td)
				} else {
					builtin = append(builtin, td)
				}
			}
		}
		tools := func(defs []model.ToolDef) int {
			if len(defs) == 0 {
				return 0
			}
			return count(model.Request{Tools: defs})
		}
		parts = []contextPart{
			{"system prompt", system},
			{"memory files", memory},
			{"tools", tools(builtin)},
			{"MCP tools", tools(mcp)},
			{"messages", count(model.Request{Messages: st.loop.Messages()})},
		}
	}
	return parts, a.Profile().ContextWindow
}

// slashContext is /context: how full the context window is, and with what.
func slashContext(ctx context.Context, e *cmdEnv, _ []string) (bool, error) {
	parts, window := contextBreakdown(e.st)
	if parts == nil {
		return false, errors.New("no model is configured")
	}
	used := 0
	for _, p := range parts {
		used += p.tokens
	}
	pct := func(n int) string {
		if window <= 0 {
			return "-"
		}
		return fmt.Sprintf("%.1f%%", float64(n)*100/float64(window))
	}
	rows := [][]string{{"part", "tokens", "of window"}}
	for _, p := range parts {
		rows = append(rows, []string{p.label, strconv.Itoa(p.tokens), pct(p.tokens)})
	}
	if window > 0 {
		rows = append(rows, []string{"free", strconv.Itoa(max(0, window-used)), pct(max(0, window-used))})
	}
	body := []ui.Block{{Kind: ui.BlockTable, Rows: rows}}
	note := fmt.Sprintf("%d of %d tokens used (%s).", used, window, pct(used))
	if window <= 0 {
		note = fmt.Sprintf("%d tokens used; the model's context window is not known.", used)
	}
	if e.st.loop == nil {
		note += " No conversation yet: the system prompt, tools and messages are counted from the first message."
	}
	if at := e.st.appCfg.Context.CompactAt; at > 0 {
		note += fmt.Sprintf(" It compacts by itself at %.0f%%; /compact [focus] does it now.", at*100)
	}
	body = append(body, ui.Block{Kind: ui.BlockNotice, Text: note})
	return false, e.ui.Panel(ctx, ui.PanelSpec{Title: "Context", Body: body})
}

// autoMemory is the session's memory_write tool when auto memory is on.
var autoMemory *tools.MemoryWrite

// withAutoMemory is the main conversation's registry with memory_write when
// the configuration turns auto memory on: the person's choice, which a
// managed value binds and a workspace may only turn off. Only an interactive
// session has it, where each save is shown as it happens.
func withAutoMemory(reg *tools.Registry, cfg config.Config, workspace string, interactive bool) *tools.Registry {
	if !cfg.Memory.Auto || !interactive {
		return reg
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return reg
	}
	autoMemory = &tools.MemoryWrite{Path: tools.AutoMemoryPath(home, workspace)}
	out := reg.Clone()
	out.Add(autoMemory)
	return out
}

// bindAutoMemory gives memory_write the conversation's redactor, and makes
// each save visible and recorded as memory.written by the agent.
func bindAutoMemory(st *cliState) {
	if autoMemory == nil || st.loop == nil {
		return
	}
	loop := st.loop
	autoMemory.Bind(func(s string) string { return redactFor(loop, s) }, func(path, kind string) error {
		shown := path
		if home, err := os.UserHomeDir(); err == nil {
			if rel, err := filepath.Rel(home, path); err == nil && filepath.IsLocal(rel) {
				shown = "~/" + filepath.ToSlash(rel)
			}
		}
		if _, err := loop.Recorder.Record(agent.EvMemoryWritten, agent.ActorAgent, agent.Untrusted,
			agent.MemoryWritten{Path: shown, Kind: kind, By: "agent"}); err != nil {
			return err
		}
		surfaceOf(st, nil).Append(ui.Block{Kind: ui.BlockNotice, Text: "the agent saved a " + kind + " note to auto memory (" + shown + "); /memory shows it"})
		return nil
	})
}

// autoMemoryQuestion is the one question onboarding asks about auto
// memory. Its default is off.
var autoMemoryQuestion = ui.DialogSpec{
	Kind:  ui.DialogConfirm,
	Title: "Let the agent keep notes between sessions (auto memory)?",
	Body: []ui.Block{{Kind: ui.BlockNotice, Text: "When on, the agent may save short notes for this workspace in ~/.abhed/projects. " +
		"Every save asks as a change, is shown and recorded, and secrets are redacted. " +
		"Text the agent reads could try to make it save something, so it is off unless you turn it on. " +
		"/memory auto on|off changes it later."}},
	Default: ui.ChoiceNo,
	Why:     "auto memory · off by default",
}

// AskAutoMemory is the onboarding hook: it asks once whether to turn auto
// memory on and keeps the answer in the person's configuration. No answer
// leaves it off. A managed value is not asked about.
func AskAutoMemory(ctx context.Context, sf ui.Surface, cfg config.Config) (bool, error) {
	if cfg.ManagedSets("memory.auto") {
		return cfg.Memory.Auto, nil
	}
	choice, err := sf.Dialog(ctx, autoMemoryQuestion)
	on := err == nil && choice == ui.ChoiceYes
	return on, setUserAutoMemory(on)
}

// setUserAutoMemory sets memory.auto in ~/.abhed/config.json, keeping every
// other setting as it is.
func setUserAutoMemory(on bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".abhed", "config.json")
	doc := map[string]any{}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the person's own configuration under their home
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("%s is not valid JSON, so it was left alone: %w", path, err)
		}
	}
	mem, _ := doc["memory"].(map[string]any)
	if mem == nil {
		mem = map[string]any{}
	}
	mem["auto"] = on
	doc["memory"] = mem
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, append(out, '\n'), 0o600)
}

// memoryAuto is /memory auto [on|off].
func memoryAuto(e *cmdEnv, args []string) error {
	cfg := e.st.appCfg
	state := "off"
	if cfg.Memory.Auto {
		state = "on"
	}
	if len(args) == 0 {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "auto memory is " + state + " in this session"})
		return nil
	}
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		return errors.New("usage: /memory auto [on|off]")
	}
	on := args[0] == "on"
	if cfg.ManagedSets("memory.auto") && on != cfg.Memory.Auto {
		return errors.New("auto memory is set by the organisation's managed configuration")
	}
	if err := setUserAutoMemory(on); err != nil {
		return err
	}
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "auto memory turned " + args[0] + " in ~/.abhed/config.json; it takes effect in the next session" +
		map[bool]string{true: " (a workspace may still turn it off)", false: ""}[on]})
	return nil
}
