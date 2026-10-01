package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A server that applies the schema at start, as a single-role one does, while
// another node appends: the schema alters sessions and then events, an append
// holds events and then checks its session. Neither may be chosen as the
// victim of a deadlock, which would lose the event or fail the start.
func TestMigrateWhileAppending(t *testing.T) {
	p := openStore(t, "migrate-race")
	var ids []string
	for i := range 4 {
		ids = append(ids, fmt.Sprintf("mr-%d-%d", time.Now().UnixNano(), i))
		newSession(t, p, ids[i], "migrate-race")
	}
	stop := time.Now().Add(3 * time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for _, id := range ids {
		wg.Go(func() {
			for seq := int64(1); time.Now().Before(stop); seq++ {
				if err := p.Append(ev(id, seq, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "x"})); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	for range 2 {
		wg.Go(func() {
			for time.Now().Before(stop) {
				if err := p.Migrate(context.Background()); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
