package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// slashCase is one slash command: it reports whether the session should end.
type slashCase func(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool

// slashCases maps each command to its handler.
var slashCases = map[string]slashCase{
	"/help":     slashHelp,
	"/mode":     slashMode,
	"/cost":     slashCost,
	"/compact":  slashCompact,
	"/sessions": slashSessions,
	"/resume":   slashResume,
	"/undo":     slashUndo,
	"/diff":     slashDiff,
	"/tasks":    slashTasks,
	"/wake":     slashWake,
	"/clear":    slashClear,
	"/memory":   slashMemory,
	"/model":    slashModel,
	"/tree":     slashTree,
	"/fork":     slashFork,
	"/hawkeye":  slashHawkeye,
	"/export":   slashExport,
	"/think":    slashThink,
	"/cwd":      slashCwd,
}

func handleCommand(ctx context.Context, line string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState) bool {
	s := r.Style()
	fields := strings.Fields(line)

	switch fields[0] {
	case "/quit", "/exit":
		return true
	}
	if run, ok := slashCases[fields[0]]; ok {
		return run(ctx, fields, r, pol, sess, st, s)
	}
	fmt.Printf("  %s unknown command %s — try /help\n", s.Red("✕"), fields[0])
	return false
}

// slashHelp is /help.
func slashHelp(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	// One list, in internal/ui: help, tab completion and the suggestion
	// menu cannot drift apart if they read the same source.
	fmt.Println(ui.HelpText(s))
	return false
}
