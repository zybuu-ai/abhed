package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// The restore test: scripts/restore-test.sh seeds a database, backs it up,
// restores it each way it documents, and runs the check phase against each
// copy. Both phases skip unless ABHED_RESTORE_PHASE names them.

const restoreTenant = "restore"

// restoreFingerprint is what a restore must keep exactly: every chained
// column, the commit order exporters read, and where a reader had got to.
type restoreFingerprint struct {
	Events   []restoreRow `json:"events"`
	Audit    []restoreRow `json:"audit"`
	Sessions []string     `json:"sessions"`
	Deleted  string       `json:"deleted"`
	Live     string       `json:"live"`
	Keys     int          `json:"keys"`
	Cursor   string       `json:"cursor"`
	Scanned  int          `json:"scanned"`
}

type restoreRow struct {
	Key      string `json:"key"`
	Pos      int64  `json:"pos"`
	Prev     string `json:"prev"`
	Hash     string `json:"hash"`
	CommitTx string `json:"commit_tx,omitempty"`
}

func restorePhase(t *testing.T, want string) string {
	t.Helper()
	if os.Getenv("ABHED_RESTORE_PHASE") != want {
		t.Skip("run by scripts/restore-test.sh")
	}
	f := os.Getenv("ABHED_RESTORE_FILE")
	if f == "" {
		t.Fatal("ABHED_RESTORE_FILE is unset")
	}
	return f
}

func readFingerprint(t *testing.T, p *Postgres) restoreFingerprint {
	t.Helper()
	ctx := context.Background()
	var fp restoreFingerprint
	rows, err := p.pool.Query(ctx, `SELECT id, chain_pos, prev_hash, hash, commit_tx::text FROM events
		WHERE tenant_id = $1 ORDER BY session_id, chain_pos`, restoreTenant)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r restoreRow
		if err := rows.Scan(&r.Key, &r.Pos, &r.Prev, &r.Hash, &r.CommitTx); err != nil {
			t.Fatal(err)
		}
		fp.Events = append(fp.Events, r)
	}
	rows.Close()
	rows, err = p.pool.Query(ctx, `SELECT seq::text, chain_pos, prev_hash, hash FROM sandbox_audit
		WHERE tenant_id = $1 ORDER BY chain_pos`, restoreTenant)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r restoreRow
		if err := rows.Scan(&r.Key, &r.Pos, &r.Prev, &r.Hash); err != nil {
			t.Fatal(err)
		}
		fp.Audit = append(fp.Audit, r)
	}
	rows.Close()
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM session_keys WHERE tenant_id = $1`, restoreTenant).Scan(&fp.Keys); err != nil {
		t.Fatal(err)
	}
	return fp
}

// scanAll reads the record in commit order from pos, as an exporter does.
func scanAll(t *testing.T, p *Postgres, pos CommitPos) (CommitPos, []string) {
	t.Helper()
	var ids []string
	for {
		n := 0
		err := p.ScanCommitted(context.Background(), pos, 100, func(e agent.Event, at CommitPos) error {
			ids = append(ids, e.ID)
			pos = at
			n++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return pos, ids
		}
	}
}

func TestRestoreSeed(t *testing.T) {
	file := restorePhase(t, "seed")
	p := runtimeStore(t, restoreTenant)
	ctx := context.Background()
	ids := []string{testID(t, "sess-rs-a-"), testID(t, "sess-rs-b-"), testID(t, "sess-rs-c-")}
	for _, id := range ids {
		newSession(t, p, id, restoreTenant)
	}
	sub := ids[0] + "-sub"
	if err := p.CreateSubagentSession(ctx, sub, ids[0], "a subagent"); err != nil {
		t.Fatal(err)
	}
	// Interleaved, so the table's order is not any one session's order.
	for i := int64(1); i <= 6; i++ {
		for _, id := range append(ids, sub) {
			if err := p.Append(ev(id, i, agent.EvAgentMessage, agent.Trusted, map[string]any{"n": i, "s": id})); err != nil {
				t.Fatal(err)
			}
		}
	}
	sb, err := p.Sandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := sb.Append(ctx, controlplane.AuditEvent{At: time.Now().UTC(), Principal: "local:ops",
			Action: fmt.Sprintf("api:POST /v1/test/%d", i), Outcome: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	// A deleted session: its rows stay, and its key goes.
	if err := p.DeleteSession(ids[2]); err != nil {
		t.Fatal(err)
	}
	cursor, scanned := scanAll(t, p, CommitPos{})
	fp := readFingerprint(t, p)
	fp.Sessions, fp.Deleted, fp.Live = append(ids, sub), ids[2], ids[1]
	fp.Cursor, fp.Scanned = cursor.String(), len(scanned)
	if len(fp.Events) != 24 || len(fp.Audit) < 5 || fp.Scanned != 24 {
		t.Fatalf("seeded %d events, %d audit rows, scanned %d", len(fp.Events), len(fp.Audit), fp.Scanned)
	}
	raw, _ := json.MarshalIndent(fp, "", " ")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreCheck(t *testing.T) {
	file := restorePhase(t, "check")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var want restoreFingerprint
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	p := runtimeStore(t, restoreTenant)
	ctx := context.Background()
	got := readFingerprint(t, p)
	diff := func(what string, a, b []restoreRow) {
		if len(a) != len(b) {
			t.Errorf("%s: %d rows after the restore, %d before", what, len(a), len(b))
			return
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("%s row %s changed: %+v, was %+v", what, b[i].Key, a[i], b[i])
				return
			}
		}
	}
	diff("events", got.Events, want.Events)
	diff("sandbox_audit", got.Audit, want.Audit)
	if got.Keys != want.Keys {
		t.Errorf("session keys: %d after, %d before", got.Keys, want.Keys)
	}
	for _, id := range want.Sessions {
		if r, err := p.VerifyChain(ctx, id); err != nil || !r.OK() {
			t.Errorf("chain of %s after the restore: %+v, %v", id, r, err)
		}
	}
	if r, err := p.VerifyAuditChain(ctx); err != nil || !r.OK() {
		t.Errorf("audit chain after the restore: %+v, %v", r, err)
	}
	// A reader that had read everything goes on from its cursor and sees
	// exactly what is written after the restore, in a live session and a new one.
	pos, err := ParseCommitPos(want.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if _, again := scanAll(t, p, pos); len(again) != 0 {
		t.Errorf("a caught-up reader sees %d old events again", len(again))
	}
	fresh := testID(t, "sess-rs-new-")
	newSession(t, p, fresh, restoreTenant)
	written := []string{}
	for i, id := range []string{want.Live, fresh} {
		e := ev(id, int64(100+i), agent.EvAgentMessage, agent.Trusted, map[string]string{"after": "restore"})
		if id == fresh {
			e = ev(id, 1, agent.EvAgentMessage, agent.Trusted, map[string]string{"after": "restore"})
		}
		if err := p.Append(e); err != nil {
			t.Fatal(err)
		}
		written = append(written, e.ID)
	}
	_, next := scanAll(t, p, pos)
	if len(next) != len(written) {
		t.Errorf("after the restore a reader at its cursor sees %d of the %d new events (%v)", len(next), len(written), next)
	}
	if r, err := p.VerifyChain(ctx, want.Live); err != nil || !r.OK() {
		t.Errorf("chain of %s after a write past the restore: %+v, %v", want.Live, r, err)
	}
	if err := p.Append(ev(want.Deleted, 50, agent.EvAgentMessage, agent.Trusted, map[string]string{"x": "y"})); err == nil {
		t.Error("an event was written into a session deleted before the backup")
	}
}
