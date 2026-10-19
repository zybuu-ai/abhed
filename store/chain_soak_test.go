package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A soak of the chained append path as a server runs it: many sessions, each
// written through its own Recorder, for ABHED_SOAK_SECONDS. With
// ABHED_SOAK_RATE the writers are held to that many events a second in all;
// without, they go as fast as the database takes them. Every event a writer
// was told was appended must be in the database, in order, in a chain that
// verifies: a dropped event is a failure, not a statistic.
//
//	ABHED_SOAK_SECONDS=30 ABHED_SOAK_SESSIONS=32 go test ./store -run TestChainSoak -v
func TestChainSoak(t *testing.T) {
	secs, _ := strconv.Atoi(os.Getenv("ABHED_SOAK_SECONDS"))
	if secs <= 0 {
		t.Skip("set ABHED_SOAK_SECONDS to run the soak")
	}
	sessions := envInt("ABHED_SOAK_SESSIONS", 32)
	rate := envInt("ABHED_SOAK_RATE", 0)
	cfg := singleRoleConfig(testDSN(t))
	if rt := os.Getenv("ABHED_TEST_RUNTIME_DSN"); rt != "" {
		runtimeStore(t, "soak") // provisions the runtime role's grants
		cfg = DefaultConfig(rt)
	}
	cfg.Tenant = testID(t, "t-soak-")
	cfg.MaxConns = int32(sessions)
	p, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ids := make([]string, sessions)
	recorded := make([]int64, sessions)
	var failed atomic.Int64
	var firstErr atomic.Value
	stop := time.Now().Add(time.Duration(secs) * time.Second)
	start := time.Now()
	var wg sync.WaitGroup
	for i := range sessions {
		ids[i] = testID(t, "sess-soak-")
		newSession(t, p, ids[i], cfg.Tenant)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := agent.NewRecorder(p, ids[i], "")
			// Each writer keeps its share of the rate to a schedule, so one that falls behind catches up.
			var every time.Duration
			if rate > 0 {
				every = time.Duration(sessions) * time.Second / time.Duration(rate)
			}
			for n := 0; time.Now().Before(stop); n++ {
				if every > 0 {
					time.Sleep(time.Until(start.Add(time.Duration(n) * every)))
				}
				_, err := rec.Record(agent.EvAgentMessage, agent.ActorAgent, agent.Trusted,
					agent.Message{Text: fmt.Sprintf("event %d of session %d", n, i)})
				if err != nil {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, err)
					return
				}
				recorded[i]++
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	var total int64
	for _, n := range recorded {
		total += n
	}
	if failed.Load() > 0 {
		t.Fatalf("%d writer(s) were refused an append: %v", failed.Load(), firstErr.Load())
	}
	ctx := context.Background()
	var held, dropped int64
	for i, id := range ids {
		r, err := p.VerifyChain(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !r.OK() {
			t.Errorf("%s: %s", id, r.Reason)
		}
		var n, maxSeq int64
		if err := p.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(seq), 0) FROM events WHERE session_id = $1`, id).Scan(&n, &maxSeq); err != nil {
			t.Fatal(err)
		}
		held += n
		if n != recorded[i] || r.Rows != recorded[i] || maxSeq != recorded[i] {
			dropped += recorded[i] - n
			t.Errorf("%s: %d appended, %d in the database, %d chained, last seq %d", id, recorded[i], n, r.Rows, maxSeq)
		}
	}
	t.Logf("soak: %d sessions, %d events in %s = %.0f events/s sustained (target %d/s, 0 is as fast as it goes); %d in the database, %d dropped, every chain verified",
		sessions, total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), rate, held, dropped)
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}
