package toolset

import (
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// context.compact_at_tokens reaches the loop; unset keeps the default.
func TestLoopConfigCompactAtTokens(t *testing.T) {
	cfg := config.Default()
	if got := LoopConfig(cfg, "").CompactAtTokens; got != agent.DefaultCompactAtTokens {
		t.Fatalf("default %d", got)
	}
	cfg.Context.CompactAtTokens = 900_000
	if got := LoopConfig(cfg, "").CompactAtTokens; got != 900_000 {
		t.Fatalf("configured %d", got)
	}
}
