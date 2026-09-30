package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

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
