package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/undo", Help: "revert the last turn's file changes", Group: "rewind", Order: 20, Run: legacy("/undo", slashUndo)})
	registerSlash(slashCmd{Name: "/diff", Help: "files changed this session", Group: "rewind", Order: 30, ReadOnly: true, Run: legacy("/diff", slashDiff)})
	registerSlash(slashCmd{Name: "/tree", Help: "show the session's steps, with the numbers /fork takes", Group: "rewind", Order: 130, ReadOnly: true, Run: legacy("/tree", slashTree)})
	registerSlash(slashCmd{Name: "/fork", Args: "[step]", Help: "rebuild the conversation up to a step and continue from it", Group: "rewind", Order: 140, Run: legacy("/fork", slashFork)})
}

// slashUndo is /undo.
func slashUndo(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	restored, err := st.undo.Undo()
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
		fmt.Printf("    %s  %s\n", s.Dim(fmt.Sprintf("%4d", ev.Seq)), label)
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
