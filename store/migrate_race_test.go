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
// another node works: an append holds events and then checks its session,
// while ClaimOrphan and Stats take sessions and then events. Whatever order
// the schema took its tables in, one of them deadlocked with it, losing an
// event, an orphan's recovery, or the start itself.
func TestMigrateWhileAppending(t *testing.T) {
	p := openStore(t, "migrate-race")
	var ids []string
	for i := range 4 {
		ids = append(ids, fmt.Sprintf("mr-%d-%d", time.Now().UnixNano(), i))
		newSession(t, p, ids[i], "migrate-race")
	}
	stop := time.Now().Add(5 * time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	migrated := 0
	var mu sync.Mutex
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
	for _, id := range ids {
		wg.Go(func() {
			for time.Now().Before(stop) {
				if _, err := p.ClaimOrphan(context.Background(), id, "node-b", time.Hour); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Go(func() {
		for time.Now().Before(stop) {
			if _, _, err := p.Stats(context.Background()); err != nil {
				errs <- err
				return
			}
		}
	})
	for range 2 {
		wg.Go(func() {
			for time.Now().Before(stop) {
				if err := p.Migrate(context.Background()); err != nil {
					errs <- err
					return
				}
				mu.Lock()
				migrated++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if migrated == 0 {
		t.Error("the schema was never applied, so nothing was tried")
	}
}
