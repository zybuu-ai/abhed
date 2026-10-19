package store

import (
	"context"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
)

// The cost of the API audit on Postgres: concurrent writers of one tenant
// share its chain lock. Opt in with ABHED_AUDIT_LOAD=1; it reports, it does
// not judge.
func TestAuditWriteLoad(t *testing.T) {
	if os.Getenv("ABHED_AUDIT_LOAD") == "" {
		t.Skip("set ABHED_AUDIT_LOAD=1 to measure the audit's write cost")
	}
	per := 200
	if n, err := strconv.Atoi(os.Getenv("ABHED_AUDIT_LOAD_WRITES")); err == nil && n > 0 {
		per = n
	}
	for _, clients := range []int{1, 5, 20} {
		tenant := testID(t, "audit-load-")
		p := openStore(t, tenant)
		ctx := context.Background()
		q, err := p.Sandboxes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var lat []time.Duration
		var wg sync.WaitGroup
		start := time.Now()
		for c := 0; c < clients; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				mine := make([]time.Duration, 0, per)
				for i := 0; i < per; i++ {
					t0 := time.Now()
					if err := q.Append(ctx, controlplane.AuditEvent{At: time.Now(), Principal: "load", Action: "api:POST /v1/sessions/{id}/messages",
						Outcome: controlplane.OutcomeRequested, Detail: map[string]string{"route": "POST /v1/sessions/{id}/messages", "path.id": "s-1"}}); err != nil {
						t.Error(err)
						return
					}
					mine = append(mine, time.Since(t0))
				}
				mu.Lock()
				lat = append(lat, mine...)
				mu.Unlock()
			}()
		}
		wg.Wait()
		wall := time.Since(start)
		slices.Sort(lat)
		pct := func(f float64) time.Duration { return lat[int(f*float64(len(lat)-1))] }
		t.Logf("clients=%d writes=%d wall=%v rate=%.0f/s p50=%v p95=%v p99=%v max=%v",
			clients, len(lat), wall.Round(time.Millisecond), float64(len(lat))/wall.Seconds(),
			pct(0.5).Round(10*time.Microsecond), pct(0.95).Round(10*time.Microsecond), pct(0.99).Round(10*time.Microsecond), lat[len(lat)-1].Round(10*time.Microsecond))
		if r, err := p.VerifyAuditChain(ctx); err != nil || !r.OK() || r.Rows != int64(len(lat)) {
			t.Fatalf("chain after load: %+v %v", r, err)
		}
	}
}
