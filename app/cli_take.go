package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/take", Args: "[worktree]", Help: "review a subagent's worktree by hunk and take what you accept into the workspace", Group: "rewind", Order: 35, Run: legacy("/take", slashTake)})
}

// Answers while reviewing a worktree's hunks.
const (
	takeYes      = "take"
	takeNo       = "skip"
	takeRest     = "rest"
	takeSkipRest = "skip-file"
	takeDone     = "done"
)

// slashTake is /take: with no argument, the subagent worktrees there are;
// with one, its changes hunk by hunk, each taken or skipped, then put into
// the workspace as your own writes, and the worktree removed if you ask.
func slashTake(ctx context.Context, fields []string, _ *ui.Renderer,
	_ *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	if st.loop == nil {
		fmt.Println(s.Dim("  send a prompt first: a take is recorded in the session"))
		return false
	}
	ws := sess.Root
	if len(fields) < 2 {
		list, err := agent.ListWorktrees(ctx, ws)
		if err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return false
		}
		if len(list) == 0 {
			fmt.Println(s.Dim("  no subagent worktrees in this workspace"))
			return false
		}
		for _, w := range list {
			detail := ""
			if ch, err := w.Changes(ctx, ws); err == nil {
				detail = fmt.Sprintf("%d file(s) changed", len(ch.Files))
			}
			fmt.Printf("  %s  %s  %s\n", s.Bold(w.ID), s.Dim(w.Branch), detail)
		}
		fmt.Println(s.Dim("  /take <id> to review one"))
		return false
	}
	w, err := agent.FindWorktree(ctx, ws, fields[1])
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	ch, err := w.Changes(ctx, ws)
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	if len(ch.Files) == 0 {
		fmt.Println(s.Dim("  the worktree changes nothing"))
	}
	accept, total, picked := map[string][]int{}, 0, 0
review:
	for _, f := range ch.Files {
		fmt.Printf("\n  %s %s\n", s.Bold(f.Path), s.Dim(f.Status))
		if f.Unsupported != "" {
			fmt.Printf("    %s not taken here: %s\n", s.Yellow("!"), f.Unsupported)
			total++
			continue
		}
		rest := ""
		for i, h := range f.Hunks {
			total++
			if rest == "" {
				printHunk(f, h, i, s)
			}
			answer := rest
			if answer == "" {
				title := fmt.Sprintf("Take change %d of %d in %s?", i+1, len(f.Hunks), f.Path)
				answer, err = st.ui().Dialog(ctx, ui.DialogSpec{Kind: ui.DialogChoice, Title: title, Default: takeNo, Cancel: takeDone,
					Choices: []ui.Choice{
						{ID: takeYes, Label: "Take it"},
						{ID: takeNo, Label: "Skip it"},
						{ID: takeRest, Label: "Take it and the rest of this file"},
						{ID: takeSkipRest, Label: "Skip the rest of this file"},
						{ID: takeDone, Label: "Stop reviewing"},
					}})
				if err != nil {
					answer = takeDone
				}
			}
			switch answer {
			case takeRest:
				rest = takeRest
				fallthrough
			case takeYes:
				accept[f.Path] = append(accept[f.Path], i)
				picked++
			case takeSkipRest:
				rest = takeSkipRest
			case takeDone:
				break review
			}
		}
	}
	if picked == 0 {
		fmt.Println(s.Dim("\n  nothing taken; the worktree is kept"))
		return false
	}
	answer, err := st.ui().Dialog(ctx, ui.DialogSpec{Kind: ui.DialogChoice, Default: "cancel",
		Title: fmt.Sprintf("Put %d of %d change(s) into the workspace?", picked, total),
		Why:   "each file is written as your change, through the permission rules, and /undo takes it back",
		Choices: []ui.Choice{
			{ID: "keep", Label: "Take them, and keep the worktree"},
			{ID: "remove", Label: "Take them, then remove the worktree and its branch (what is not taken is lost)", Destructive: true},
			{ID: "cancel", Label: "Cancel"},
		}})
	if err != nil || answer == "cancel" {
		fmt.Println(s.Dim("  nothing taken"))
		return false
	}
	res, err := st.loop.Take(ctx, ws, w, accept, answer == "remove", st.loop.ManualTaker(sess))
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	printTake(res, w, s)
	return false
}

// printHunk shows one change as a unified diff, with the base's lines around it.
func printHunk(f agent.WorktreeFile, h agent.WorktreeHunk, i int, s ui.Style) {
	if f.Whole {
		fmt.Printf("    %s\n", s.Dim("taken whole: a binary or large file"))
		return
	}
	fmt.Printf("    %s\n", s.Dim(fmt.Sprintf("@@ -%d,%d +%d,%d @@ change %d", h.OldStart+1, h.OldCount, h.NewStart+1, h.NewCount, i+1)))
	line := func(prefix string, l string, paint func(string) string) {
		fmt.Printf("    %s\n", paint(prefix+strings.TrimRight(l, "\n")))
	}
	for _, l := range h.Before {
		line(" ", l, s.Dim)
	}
	for _, l := range h.Old {
		line("-", l, s.DiffDel)
	}
	for _, l := range h.New {
		line("+", l, s.DiffAdd)
	}
	for _, l := range h.After {
		line(" ", l, s.Dim)
	}
}

func printTake(res agent.TakeResult, w agent.Worktree, s ui.Style) {
	for _, p := range res.Taken {
		fmt.Printf("  %s took %s\n", s.Green("✓"), p)
	}
	for _, c := range res.Conflicts {
		fmt.Printf("  %s conflict: %s; left as it is\n", s.Yellow("!"), c)
	}
	for _, r := range res.Refused {
		fmt.Printf("  %s not taken: %s\n", s.Red("✕"), r)
	}
	switch {
	case res.Removed:
		fmt.Printf("  %s\n", s.Dim("removed the worktree "+w.Rel+" and its branch "+w.Branch))
	case res.Left > 0:
		fmt.Printf("  %s\n", s.Dim(fmt.Sprintf("%d change(s) not taken stay in %s; /take %s again, or remove it", res.Left, w.Rel, w.ID)))
	default:
		fmt.Printf("  %s\n", s.Dim("the worktree "+w.Rel+" is kept; /take "+w.ID+" again to remove it"))
	}
}
