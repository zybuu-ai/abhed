package app

import (
	"context"
	"fmt"

	"github.com/zybuu-ai/abhed/hawkeye"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// slashCost is /cost.
func slashCost(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	u := st.total
	if u.InputTokens == 0 {
		fmt.Println(s.Dim("  no usage yet this session"))
		return false
	}
	fmt.Printf("  turns          %d\n", u.Turns)
	fmt.Printf("  tokens in      %d\n", u.InputTokens)
	fmt.Printf("  tokens out     %d\n", u.OutputTokens)
	fmt.Printf("  cached         %d (%.0f%%)\n", u.CachedTokens, u.CacheHitRate()*100)
	if savings := u.PrefillSavings(); savings > 0 {
		fmt.Printf("  prefill saving %.1fx\n", savings)
	}
	fmt.Printf("  compactions    %d\n", u.Compactions)
	return false
}

// slashHawkeye is /hawkeye.
func slashHawkeye(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	// The summary goes to the terminal; a path writes the full report.
	events, err := st.store.Events(st.sessionID)
	if err != nil || len(events) == 0 {
		fmt.Println(s.Dim("  nothing recorded yet"))
		return false
	}
	rep := hawkeye.Analyze(st.sessionID, events)
	fmt.Print(hawkeye.Text(rep))
	if len(fields) > 1 {
		if err := writeHawkeye(fields[1], rep); err != nil {
			fmt.Printf("  %s %v\n", s.Red("✕"), err)
			return false
		}
		fmt.Printf("\n  wrote %s\n", fields[1])
	}
	return false
}

// slashThink is /think.
func slashThink(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	// Reasoning is collapsed to a word count by default because on a model
	// that reasons at length it buries the answer. This turns the full
	// text on for the session.
	r.Reasoning = !r.Reasoning
	if r.Reasoning {
		// Show the block the user just saw collapsed, not only the next
		// one. A toggle that takes effect a turn later reads as broken to
		// someone looking at a summary line right now.
		if !r.ShowLastReasoning() {
			fmt.Println(s.Dim("  reasoning shown in full from the next turn"))
		}
	} else {
		fmt.Println(s.Dim("  reasoning collapsed to a summary line"))
	}
	return false
}

// slashCwd is /cwd.
func slashCwd(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	fmt.Printf("  %s\n", sess.Root)
	return false
}
