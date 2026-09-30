package app

import (
	"context"
	"fmt"

	"github.com/zybuu-ai/abhed/hawkeye"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/usage", Aliases: []string{"/cost"}, Help: "tokens, cache hit rate, prefill saving, and by subagent and tool source", Group: "status", Order: 40, ReadOnly: true, Run: slashUsage})
	registerSlash(slashCmd{Name: "/status", Help: "model, mode, sandbox, record, trust and limits at a glance", Group: "status", Order: 30, ReadOnly: true, Run: slashStatus})
	registerSlash(slashCmd{Name: "/config", Args: "[set <key> <value>]", Help: "your settings and where each comes from; set changes your own file", Group: "status", Order: 35, Run: slashConfig})
	registerSlash(slashCmd{Name: "/hawkeye", Args: "[path]", Help: "what this session did: tokens, policy decisions, findings", Group: "status", Order: 160, Run: legacy("/hawkeye", slashHawkeye)})
	registerSlash(slashCmd{Name: "/think", Help: "show or collapse the model's reasoning", Group: "status", Order: 170, Run: legacy("/think", slashThink)})
	registerSlash(slashCmd{Name: "/cwd", Help: "show the workspace root", Group: "status", Order: 180, ReadOnly: true, Run: legacy("/cwd", slashCwd)})
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
