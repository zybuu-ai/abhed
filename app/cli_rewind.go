package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secretfiles"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/store/local"
)

func init() {
	registerSlash(slashCmd{Name: "/undo", Help: "revert the last turn's file changes", Group: "rewind", Order: 20, Run: legacy("/undo", slashUndo)})
	registerSlash(slashCmd{Name: "/rewind", Args: "[n]", Help: "take code and/or the conversation back to before a prompt; recorded, nothing deleted", Group: "rewind", Order: 25, Run: legacy("/rewind", slashRewind)})
	registerSlash(slashCmd{Name: "/diff", Help: "files changed this session", Group: "rewind", Order: 30, ReadOnly: true, Run: legacy("/diff", slashDiff)})
	registerSlash(slashCmd{Name: "/tree", Help: "show the session's steps, with the numbers /fork takes", Group: "rewind", Order: 130, ReadOnly: true, Run: legacy("/tree", slashTree)})
	registerSlash(slashCmd{Name: "/fork", Args: "[step]", Help: "rebuild the conversation up to a step and continue from it", Group: "rewind", Order: 140, Run: legacy("/fork", slashFork)})
}

// slashUndo is /undo: code only, back to before the last turn that changed
// files. Each file put back is recorded as file.restored.
func slashUndo(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	since, recorded := st.undo.LastTurnStart()
	if !recorded || st.loop == nil {
		if st.undo.Pending() == 0 {
			fmt.Printf("  %s nothing to undo\n", s.Red("✕"))
			return false
		}
		fmt.Printf("  %s the last turn's checkpoints are not in the record; nothing was changed\n", s.Red("✕"))
		return false
	}
	restored, err := rewindCode(ctx, st, sess, since)
	for _, line := range restored {
		fmt.Printf("  %s\n", line)
	}
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	fmt.Printf("  %s\n", s.Dim(fmt.Sprintf("%d turn(s) still undoable", st.undo.Pending())))
	return false
}

// Rewind choices.
const (
	rewindBoth  = "both"
	rewindChat  = "conversation"
	rewindFiles = "code"
)

// slashRewind is /rewind: pick a prompt, then take the code, the
// conversation or both back to just before it. The conversation side is a
// recorded fork, never a deletion; each file put back is file.restored.
func slashRewind(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	if st.loop == nil || st.sessionID == "" {
		fmt.Println(s.Dim("  nothing to rewind yet"))
		return false
	}
	events, err := st.store.Events(st.sessionID)
	if err != nil || len(events) == 0 {
		fmt.Println(s.Dim("  nothing to rewind yet"))
		return false
	}
	points := agent.RewindPoints(events)
	if len(points) == 0 {
		fmt.Println(s.Dim("  nothing to rewind yet"))
		return false
	}
	var point agent.RewindPoint
	if len(fields) > 1 {
		// /rewind n: the nth prompt back, 1 the latest.
		n, err := strconv.Atoi(fields[1])
		if err != nil || n < 1 || n > len(points) {
			fmt.Printf("  %s /rewind takes 1 to %d, the prompts back from the latest\n", s.Red("✕"), len(points))
			return false
		}
		point = points[len(points)-n]
	} else {
		items := make([]ui.PickItem, 0, len(points))
		for i := len(points) - 1; i >= 0; i-- {
			p := points[i]
			detail := fmt.Sprintf("step %d", p.Seq)
			if n := st.undo.Peek(p.Seq); n > 0 {
				detail += fmt.Sprintf(" · %d file change(s) after it", n)
			}
			items = append(items, ui.PickItem{ID: "at-" + strconv.FormatInt(p.Seq, 10), Label: firstLine(p.Text, 70), Detail: detail})
		}
		id, err := st.ui().Pick(ctx, ui.PickSpec{Title: "Rewind to before which prompt?", Items: items})
		if err != nil {
			fmt.Println(s.Dim("  not rewound"))
			return false
		}
		for _, p := range points {
			if "at-"+strconv.FormatInt(p.Seq, 10) == id {
				point = p
			}
		}
	}
	choices := []ui.Choice{
		{ID: rewindChat, Label: "Conversation only"},
		{ID: "cancel", Label: "Cancel"},
	}
	if st.undo.Peek(point.Seq) > 0 {
		choices = append([]ui.Choice{
			{ID: rewindBoth, Label: "Code and conversation"},
			{ID: rewindFiles, Label: "Code only"},
		}, choices...)
	}
	answer, err := st.ui().Dialog(ctx, ui.DialogSpec{Kind: ui.DialogChoice, Default: "cancel", Choices: choices,
		Title: fmt.Sprintf("Rewind to before %q?", firstLine(point.Text, 60)),
		Why:   "the conversation is forked, and the steps after it stay in the record; files are put back from their checkpoints"})
	if err != nil || answer == "cancel" {
		fmt.Println(s.Dim("  not rewound"))
		return false
	}
	if answer == rewindBoth || answer == rewindFiles {
		restored, err := rewindCode(ctx, st, sess, point.Seq-1)
		for _, line := range restored {
			fmt.Printf("  %s\n", line)
		}
		if err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
		}
	}
	if answer == rewindBoth || answer == rewindChat {
		if err := rewindConversation(ctx, st, point.Seq); err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return false
		}
		fmt.Printf("  %s\n", s.Dim("the conversation is back to before that prompt; it was:"))
		fmt.Printf("  %s\n", firstLine(point.Text, 200))
	}
	return false
}

// rewindCode puts back the files the agent changed after step since, as
// the person's action, under a claim on the session.
func rewindCode(ctx context.Context, st *cliState, sess *tools.Session, since int64) ([]string, error) {
	release, err := claimForWrite(ctx, st)
	if err != nil {
		return nil, err
	}
	defer release()
	cps := st.undo.Since(since)
	// Read through the session: a record's checkpoint names a path, and a
	// changed record must not reach a file outside the session's roots.
	current := func(path string) ([]byte, bool) {
		data, err := sess.ReadFile(path)
		return data, err == nil
	}
	restore := func(path string, data []byte, existed bool, mode os.FileMode) error {
		if !existed {
			if err := sess.RemoveFile(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		// The file's own mode comes back with its content, set inside the
		// session's confined write, so no link is followed.
		return sess.RestoreFileMode(path, data, mode)
	}
	var blobs agent.BlobPutter
	if rec, ok := st.store.(*local.Store); ok {
		blobs = rec.Blobs()
	}
	return st.loop.RestoreCheckpoints(cps, current, restore, blobs)
}

// rewindConversation forks the conversation to just before step seq.
func rewindConversation(ctx context.Context, st *cliState, seq int64) error {
	release, err := claimForWrite(ctx, st)
	if err != nil {
		return err
	}
	defer release()
	events, err := st.store.Events(st.sessionID)
	if err != nil {
		return err
	}
	_, err = st.loop.ForkBefore(events, seq)
	return err
}

// checkpointSaver records each checkpoint in the session's record as the
// agent is about to change a file: the content in the record's blobs, and
// checkpoint.saved naming it, so undo and rewind survive the process.
func checkpointSaver(st *cliState) func(agent.Checkpoint) (agent.Checkpoint, error) {
	return checkpointSaverFor(func() *agent.Loop { return st.loop }, st.store)
}

// checkpointSaverFor saves checkpoints for the conversation loop names, in
// the record es keeps: blobs where it has them, a hash where it does not.
func checkpointSaverFor(loopOf func() *agent.Loop, es agent.Store) func(agent.Checkpoint) (agent.Checkpoint, error) {
	return func(cp agent.Checkpoint) (agent.Checkpoint, error) {
		loop := loopOf()
		if loop == nil {
			return cp, nil
		}
		if info, err := os.Lstat(cp.Path); err == nil && info.Mode().IsRegular() {
			cp.Mode = info.Mode().Perm()
		}
		// A file policy keeps from being read, or one that holds keys, is
		// not copied into the record: its checkpoint says so, and it is not
		// offered for restore.
		if why := noCheckpoint(loop, cp.Path); cp.Existed && why != "" {
			cp.Skipped, cp.Before = why, nil
			ev, err := loop.Recorder.Record(agent.EvCheckpoint, agent.ActorSystem, agent.Trusted,
				agent.CheckpointSaved{Path: cp.Path, Turn: cp.Turn, Mode: uint32(cp.Mode), Skipped: why})
			cp.Seq = ev.Seq
			return cp, err
		}
		if cp.Existed {
			if rec, ok := es.(*local.Store); ok {
				sha, err := rec.Blobs().Put(cp.Before)
				if err != nil {
					return cp, err
				}
				cp.Blob = sha
			} else {
				cp.Blob = hashHex(cp.Before)
			}
		}
		ev, err := loop.Recorder.Record(agent.EvCheckpoint, agent.ActorSystem, agent.Trusted,
			agent.CheckpointSaved{Path: cp.Path, SHA256: cp.Blob, Turn: cp.Turn, Mode: uint32(cp.Mode)})
		if err != nil {
			return cp, err
		}
		cp.Seq = ev.Seq
		return cp, nil
	}
}

// noCheckpoint says why a file's content is not kept before an edit, "" when
// it is: policy keeps it from being read, or its name says it holds keys.
func noCheckpoint(loop *agent.Loop, path string) string {
	if loop != nil && loop.Policy != nil {
		args, _ := json.Marshal(map[string]string{"path": path})
		if d := loop.Policy.Evaluate("read", false, args); d.Decision == policy.Deny {
			return "policy denies reading it: " + d.Reason
		}
	}
	if secretfiles.Match(path) != "" {
		return "it looks like a file of keys or credentials"
	}
	return ""
}

// rebuildUndo makes the undo log the one session events recorded.
func rebuildUndo(st *cliState, events []agent.Event) {
	var get func(string) ([]byte, error)
	if rec, ok := st.store.(*local.Store); ok {
		get = rec.Blobs().Get
	}
	st.undo.Rebuild(events, get)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// slashDiff is /diff.
func slashDiff(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	changed := st.undo.Changed()
	if len(changed) == 0 {
		fmt.Println(s.Dim("  no files changed this session"))
		return false
	}
	for _, path := range changed {
		rel := path
		if r, err := filepath.Rel(sess.Root, path); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		before, existed, _ := st.undo.Original(path)
		after, readErr := sess.ReadFile(path)
		switch {
		case !existed:
			fmt.Printf("  %s %s\n", s.Green("+"), rel)
		case readErr != nil:
			fmt.Printf("  %s %s (deleted)\n", s.Red("-"), rel)
		default:
			added, removed := lineDelta(string(before), string(after))
			fmt.Printf("  %s %s  %s %s\n", s.Yellow("~"), rel,
				s.Green(fmt.Sprintf("+%d", added)), s.Red(fmt.Sprintf("-%d", removed)))
		}
	}
	return false
}

// slashTree is /tree.
func slashTree(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	// Show the session as steps, so a user can see where it went wrong
	// before deciding where to fork. Without it, /fork asks for a number
	// nobody has any way to know.
	events, err := st.store.Events(st.sessionID)
	if err != nil || len(events) == 0 {
		fmt.Println(s.Dim("  nothing recorded yet"))
		return false
	}
	forkPoints(r, agent.Live(events))
	fmt.Printf("  %s\n", s.Dim("/fork <step> rebuilds the conversation up to a step"))
	return false
}

// slashFork is /fork.
func slashFork(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	// Rebuild the conversation from the event log up to a point and carry
	// on from there. A wrong turn three steps back should cost three
	// steps, not the session: everything before it was still right, and
	// re-establishing it means paying for the same reading twice.
	events, err := st.store.Events(st.sessionID)
	if err != nil || len(events) == 0 {
		fmt.Println(s.Dim("  nothing to fork from yet"))
		return false
	}
	if len(fields) < 2 {
		fmt.Println(s.Dim("  /fork <step> — rebuild the conversation up to a step and continue from it"))
		forkPoints(r, agent.Live(events))
		return false
	}
	seq, convErr := strconv.ParseInt(fields[1], 10, 64)
	if convErr != nil {
		fmt.Printf("  %s %q is not a step number\n", s.Red("✕"), fields[1])
		return false
	}
	if st.loop == nil {
		fmt.Println(s.Dim("  no active session to fork into"))
		return false
	}
	release, claimErr := claimForWrite(ctx, st)
	if claimErr != nil {
		fmt.Printf("  %s not continued: %v\n", s.Red("✕"), claimErr)
		return false
	}
	// Read again: the claim may have rebuilt from a record another process extended.
	if fresh, err := st.store.Events(st.sessionID); err == nil {
		events = fresh
	}
	kept, forkErr := st.loop.ForkTo(events, seq)
	release()
	if forkErr != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), forkErr)
		return false
	}
	fmt.Printf("  %s\n", s.Dim(fmt.Sprintf(
		"forked at step %d — %d messages kept; the next thing you type continues from there", seq, kept)))
	return false
}

// lineDelta counts added and removed lines between two versions, for /diff.
func lineDelta(before, after string) (added, removed int) {
	b := strings.Split(before, "\n")
	a := strings.Split(after, "\n")
	counts := map[string]int{}
	for _, line := range b {
		counts[line]++
	}
	for _, line := range a {
		if counts[line] > 0 {
			counts[line]--
		} else {
			added++
		}
	}
	for _, n := range counts {
		removed += n
	}
	return added, removed
}

// forkPoints lists the steps a session can be forked at, so the user has
// something to name rather than guessing a sequence number.
func forkPoints(r *ui.Renderer, events []agent.Event) {
	s := r.Style()
	shown := 0
	for _, ev := range events {
		var label string
		switch ev.Type {
		case agent.EvUserMessage:
			var m agent.Message
			if json.Unmarshal(ev.Payload, &m) == nil {
				label = "you: " + firstLine(m.Text, 60)
			}
		case agent.EvActionRequested:
			var a agent.ActionRequested
			if json.Unmarshal(ev.Payload, &a) == nil {
				label = a.Tool + " " + firstLine(string(a.Args), 50)
			}
		default:
			continue
		}
		fmt.Printf("    %s  %s\n", s.Dim(fmt.Sprintf("%4d", ev.Seq)), ui.VisibleLine(label))
		shown++
		if shown >= 30 {
			fmt.Println(s.Dim("    …"))
			break
		}
	}
}

func firstLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
