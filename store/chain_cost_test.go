package store

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// The cost of one append, measured where noise from other load cannot reach
// it: the WAL bytes each event writes, and the time the chain trigger spends
// on it (with track_functions on). ABHED_SOAK_SETUP_SQL, as for the soak,
// measures a change before it ships.
//
//	ABHED_APPEND_COST=3000 go test ./store -run TestChainAppendCost -v
func TestChainAppendCost(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("ABHED_APPEND_COST"))
	if n <= 0 {
		t.Skip("set ABHED_APPEND_COST to the number of events to measure")
	}
	owner := openStore(t, "default")
	ctx := context.Background()
	if q := os.Getenv("ABHED_SOAK_SETUP_SQL"); q != "" {
		if _, err := owner.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	p := owner
	if os.Getenv("ABHED_TEST_RUNTIME_DSN") != "" {
		cfg := DefaultConfig(os.Getenv("ABHED_TEST_RUNTIME_DSN"))
		var err error
		if p, err = Open(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		defer p.Close()
	}
	id := testID(t, "sess-cost-")
	newSession(t, p, id, "default")
	text := strings.Repeat("a reply of ordinary length, ", 20)
	fnTime := func() (calls int64, ms float64) {
		_ = owner.pool.QueryRow(ctx, `SELECT COALESCE(sum(calls), 0), COALESCE(sum(self_time), 0) FROM pg_stat_user_functions
			WHERE funcname IN ('abhed_chain_insert', 'abhed_events_chain')`).Scan(&calls, &ms)
		return
	}
	_, _ = owner.pool.Exec(ctx, `SELECT pg_stat_clear_snapshot()`)
	var lsn0, lsn1 int64
	c0, ms0 := fnTime()
	if err := owner.pool.QueryRow(ctx, `SELECT pg_current_wal_lsn() - '0/0'::pg_lsn`).Scan(&lsn0); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := int64(1); i <= int64(n); i++ {
		if err := p.Append(ev(id, i, agent.EvAgentMessage, agent.Trusted, agent.Message{Text: text})); err != nil {
			t.Fatal(err)
		}
	}
	took := time.Since(start)
	if err := owner.pool.QueryRow(ctx, `SELECT pg_current_wal_lsn() - '0/0'::pg_lsn`).Scan(&lsn1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond) // function statistics are reported when a backend goes idle
	_, _ = owner.pool.Exec(ctx, `SELECT pg_stat_clear_snapshot()`)
	c1, ms1 := fnTime()
	trig := "not tracked (track_functions is off)"
	if c1 > c0 {
		trig = strconv.FormatFloat((ms1-ms0)*1000/float64(c1-c0), 'f', 1, 64) + " µs of trigger a row"
	}
	t.Logf("append cost: %d events, %.0f µs each from here, %d WAL bytes each (other writers included), %s",
		n, float64(took.Microseconds())/float64(n), (lsn1-lsn0)/int64(n), trig)
}
