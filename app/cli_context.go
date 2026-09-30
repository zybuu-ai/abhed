package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/compact", Args: "[hint]", Help: "compact the context now", Group: "context", Order: 50, Run: legacy("/compact", slashCompact)})
	registerSlash(slashCmd{Name: "/memory", Help: "show the ABHED.md files in effect", Group: "context", Order: 90, ReadOnly: true, Run: legacy("/memory", slashMemory)})
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

// slashMemory is /memory.
func slashMemory(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	files := agent.DiscoverMemoryFiles(sess.Root)
	if len(files) == 0 {
		path := filepath.Join(sess.Root, "ABHED.md")
		fmt.Printf("  %s\n", s.Dim("no memory file yet; create "+path))
		fmt.Printf("  %s\n", s.Dim("it is re-injected on every request, so keep it short"))
		return false
	}
	for _, f := range files {
		data, err := agent.ReadMemoryFile(sess.Root, f)
		if err != nil {
			continue
		}
		fmt.Printf("  %s %s\n", s.Bold(f), s.Dim(fmt.Sprintf("(%d bytes)", len(data))))
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			fmt.Printf("    %s\n", line)
		}
	}
	return false
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
