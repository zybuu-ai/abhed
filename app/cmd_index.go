package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// buildIndexCmd implements `abhed index`: it builds the index and keeps its
// embeddings, so a session start embeds only the chunks that changed; the
// rest of the index is not kept.
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
	// Only the embeddings are kept: the symbol and BM25 tiers take moments to rebuild.
	if vectors == 0 {
		fmt.Printf("  nothing kept: with no embeddings, each session rebuilds the symbol and BM25 tiers in moments; set retrieval.embed to add the vector tier\n")
	} else if path := toolset.VectorCachePath(cfg, workspace); path != "" {
		fmt.Printf("  embeddings kept in %s for the next session; each session rebuilds the symbol and BM25 tiers\n", config.Printable(path))
	}
	return 0
}
