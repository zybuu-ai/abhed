package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// What a server taking over abhed serve's OCSF file asks the database, on a
// record of ABHED_OCSF_TAKEOVER_SESSIONS sessions of ABHED_OCSF_TAKEOVER_EVENTS
// events each, against the scan of every session's last event it used to run.
// It reproduces the numbers in docs/guide/30-ocsf-and-logs.md (19 Oct 2026:
// 10,000 sessions, 100 events each, Postgres 17 in a 4-CPU Podman VM on a Mac):
//
//	ABHED_OCSF_TAKEOVER_SESSIONS=10000 ABHED_OCSF_TAKEOVER_EVENTS=100 go test ./store -run TestOCSFTakeoverCost -v
func TestOCSFTakeoverCost(t *testing.T) {
	sessions, _ := strconv.Atoi(os.Getenv("ABHED_OCSF_TAKEOVER_SESSIONS"))
	if sessions <= 0 {
		t.Skip("set ABHED_OCSF_TAKEOVER_SESSIONS to measure the OCSF writer's takeover")
	}
	events := envInt("ABHED_OCSF_TAKEOVER_EVENTS", 100)
	ctx := context.Background()
	tenant := testID(t, "t-ocsf-")
	p := openStore(t, tenant)
	start := time.Now()
	for from := 0; from < sessions; from += 100 {
		to := min(from+99, sessions-1)
		if _, err := p.pool.Exec(ctx, `INSERT INTO sessions (id, tenant_id, user_id, workspace, model)
			SELECT $1||g, $2, 'u', '/w', 'm' FROM generate_series($3::int, $4::int) g`, tenant+"-s-", tenant, from, to); err != nil {
			t.Fatal(err)
		}
		if _, err := p.pool.Exec(ctx, `INSERT INTO events (id, session_id, tenant_id, seq, type, payload, actor, trust)
			SELECT $1||g||'-'||q, $1||g, $2, q, 'user.message', '{"text":"m"}'::jsonb, 'user', 'trusted'
			FROM generate_series($3::int, $4::int) g, generate_series(1, $5::int) q`, tenant+"-s-", tenant, from, to, events); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.pool.Exec(ctx, `ANALYZE events`); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d sessions and %d events in %v", sessions, sessions*events, time.Since(start).Round(time.Millisecond))

	// The writer that had written everything: its cursors and its place.
	w := p.OCSFWriter("takeover")
	cur := make(map[string]OCSFCursor, sessions)
	for i := 0; i < sessions; i++ {
		cur[fmt.Sprintf("%s-s-%d", tenant, i)] = OCSFCursor{Seq: int64(events), Primed: true}
	}
	if err := w.Save(cur); err != nil {
		t.Fatal(err)
	}
	end, err := p.CommitEnd(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SavePos(end.String()); err != nil {
		t.Fatal(err)
	}

	best := func(f func() error) time.Duration {
		var out time.Duration
		for i := 0; i < 5; i++ {
			s := time.Now()
			if err := f(); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(s); i == 0 || d < out {
				out = d
			}
		}
		return out
	}
	// Before: the last event of every session, and every cursor, compared.
	scanAll := best(func() error {
		rows, err := p.pool.Query(ctx, `SELECT e.session_id, max(e.seq) FROM events e
			WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = e.session_id AND s.deleted_at IS NOT NULL)
			GROUP BY e.session_id`)
		if err != nil {
			return err
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if n != sessions {
			return fmt.Errorf("%d sessions, want %d", n, sessions)
		}
		_, _, err = w.Load(nil)
		return err
	})
	// After: whether the sink began, its place, and what was committed since.
	resume := best(func() error {
		if _, begun, err := w.Load([]string{}); err != nil || !begun {
			return fmt.Errorf("begun %v: %w", begun, err)
		}
		saved, err := w.LoadPos()
		if err != nil {
			return err
		}
		pos, err := ParseCommitPos(saved)
		if err != nil {
			return err
		}
		return p.ScanCommitted(ctx, pos, 500, func(agent.Event, CommitPos) error { return nil })
	})
	t.Logf("takeover's database work, best of 5: every session and cursor compared %v; resuming from the commit place %v",
		scanAll.Round(10*time.Microsecond), resume.Round(10*time.Microsecond))
}
