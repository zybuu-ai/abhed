package app

import (
	"context"
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
	registerSlash(slashCmd{Name: "/compact", Args: "[hint]", Help: "compact the context now", Group: "context", Order: 50, Run: legacy("/compact", slashCompact)})
	registerSlash(slashCmd{Name: "/init", Args: "[notes]", Help: "have the agent write ABHED.md from the repository", Group: "context", Order: 85, Run: slashInit})
	registerSlash(slashCmd{Name: "/memory", Args: "[show <n>|add <project|local|user> <note>]", Help: "show the ABHED.md files in effect", Group: "context", Order: 90, Run: slashMemory})
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
	info, err := st.loop.Compact(ctx)
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
