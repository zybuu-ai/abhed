package app

import (
	"io"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// Events recorded before the CLI follows a conversation never reach its
// subscription, so they count as drawn: the first turn's wait for them
// returns at once instead of running out its second.
func TestFollowCountsWhatWasRecordedAsDrawn(t *testing.T) {
	var c cliState
	c.follow(agent.NewMemStore(), "s-follow", ui.NewRenderer(io.Discard, false), func() int64 { return 3 })
	defer c.unfollow()
	start := time.Now()
	c.waitRendered(3)
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("waited %v for events recorded before following", took)
	}
}
