package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// buildIndexCmd implements `abhed index`, so a large repo can be indexed once
// rather than on every session start.
func buildIndexCmd(workspace string, trust config.TrustChoice) int {
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust})
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	fmt.Printf("indexing %s...\n", workspace)
	start := time.Now()

	ix, err := toolset.OpenIndex(context.Background(), cfg, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	docs, terms, vectors, _ := ix.Stats()
	fmt.Printf("  %d chunks · %d terms · %d vectors · %s\n",
		docs, terms, vectors, time.Since(start).Round(time.Millisecond))
	if vectors == 0 {
		fmt.Printf("  (symbol + BM25 tiers only; set retrieval.embed to add the vector tier)\n")
	}
	return 0
}
