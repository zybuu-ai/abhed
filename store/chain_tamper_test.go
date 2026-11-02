package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store/chain"
)

// chainedSession is a session of n events in a tenant of its own, so a test
// that tampers with it as the owner leaves every other test's rows alone.
func chainedSession(t *testing.T, n int) (*Postgres, string) {
	t.Helper()
	tenant := testID(t, "t-chain-")
	p := openStore(t, tenant)
	id := testID(t, "sess-tamper-")
	newSession(t, p, id, tenant)
	for i := int64(1); i <= int64(n); i++ {
		payload := map[string]any{"text": fmt.Sprintf("step %d — ünïcode, \"quotes\" and \\ slashes", i), "n": 1.50, "big": 12345678901234567}
		if err := p.Append(ev(id, i, agent.EvAgentMessage, agent.Trusted, payload)); err != nil {
			t.Fatal(err)
		}
	}
	return p, id
}

// asOwner runs statements in one transaction as the tables' owner, with the
// named triggers off: what only an owner can do.
func asOwner(t *testing.T, p *Postgres, triggers []string, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, tr := range triggers {
		stmts = append([]string{`ALTER TABLE events DISABLE TRIGGER ` + tr}, stmts...)
		stmts = append(stmts, `ALTER TABLE events ENABLE TRIGGER `+tr)
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// The hash recomputed here matches the one the trigger wrote, for payloads
// whose rendering is easy to get wrong, and the database's own check agrees.
func TestGoVerifierAgreesWithTheTrigger(t *testing.T) {
	p, id := chainedSession(t, 5)
	ctx := context.Background()
	r, err := p.VerifyChain(ctx, id)
	if err != nil || !r.OK() || r.Rows != 5 {
		t.Fatalf("verify = %+v, %v", r, err)
	}
	var bad, rows int64
	var head string
	if err := p.pool.QueryRow(ctx, `SELECT first_bad, rows_chained, head FROM abhed_chain_verify('events', 'session_id', $1, $2)`,
		id, eventChainColumns).Scan(&bad, &rows, &head); err != nil {
		t.Fatal(err)
	}
	if bad != 0 || rows != 5 || head != r.Head {
		t.Fatalf("database says %d rows, head %s, first bad %d; Go says %s", rows, head, bad, r.Head)
	}
}

// B4 in the database: each tamper an owner can make behind the triggers is
// found, and the cuts are found against a head kept before them.
func TestOwnerTamperIsDetected(t *testing.T) {
	ctx := context.Background()
	type tamper struct {
		triggers []string
		stmts    func(id string) []string
		// at is the first bad position VerifyChain reports; 0 when only a kept head finds it.
		at int64
	}
	cases := map[string]tamper{
		"edit a payload": {[]string{"events_no_update"}, func(id string) []string {
			return []string{`UPDATE events SET payload = '{"text":"nothing happened"}' WHERE session_id = '` + id + `' AND seq = 3`}
		}, 3},
		"delete a row": {[]string{"events_no_delete"}, func(id string) []string {
			return []string{`DELETE FROM events WHERE session_id = '` + id + `' AND seq = 3`}
		}, 3},
		"reorder two rows": {[]string{"events_no_update"}, func(id string) []string {
			w := `session_id = '` + id + `'`
			return []string{
				`UPDATE events SET chain_pos = -1 WHERE ` + w + ` AND chain_pos = 2`,
				`UPDATE events SET chain_pos = 2 WHERE ` + w + ` AND chain_pos = 3`,
				`UPDATE events SET chain_pos = 3 WHERE ` + w + ` AND chain_pos = -1`,
			}
		}, 2},
		"cut the end": {[]string{"events_no_delete"}, func(id string) []string {
			return []string{`DELETE FROM events WHERE session_id = '` + id + `' AND seq > 4`}
		}, 0},
		"truncate the session": {[]string{"events_no_delete"}, func(id string) []string {
			return []string{`DELETE FROM events WHERE session_id = '` + id + `'`}
		}, 0},
		"rechain after an edit": {[]string{"events_no_update"}, func(id string) []string {
			w := `session_id = '` + id + `'`
			out := []string{`UPDATE events SET payload = '{"text":"nothing happened"}' WHERE ` + w + ` AND seq = 2`}
			for pos := 2; pos <= 6; pos++ {
				out = append(out, fmt.Sprintf(`UPDATE events t SET
					prev_hash = (SELECT hash FROM events WHERE %[1]s AND chain_pos = %[2]d - 1),
					hash = abhed_chain_hash((SELECT hash FROM events WHERE %[1]s AND chain_pos = %[2]d - 1), to_jsonb(t), '%[3]s')
					WHERE %[1]s AND chain_pos = %[2]d`, w, pos, "{"+strings.Join(eventChainColumns, ",")+"}"))
			}
			return out
		}, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p, id := chainedSession(t, 6)
			src := p.EventChain()
			kept, err := src.Head(ctx, id)
			if err != nil || kept.Pos != 6 || kept.Chain != "postgres/events" || kept.Scope != id {
				t.Fatalf("head = %+v, %v", kept, err)
			}
			asOwner(t, p, c.triggers, c.stmts(id)...)
			r, err := p.VerifyChain(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if c.at > 0 && (r.OK() || r.FirstBad != c.at) {
				t.Fatalf("verify = %+v, want first bad %d", r, c.at)
			}
			if c.at == 0 && !r.OK() {
				t.Fatalf("verify = %+v; this tamper leaves a whole chain, found only by a kept head", r)
			}
			// Against the head kept before: the range to it no longer holds, or ends elsewhere.
			rg, err := src.VerifyRange(ctx, id, 1, kept.Pos)
			if err != nil {
				t.Fatal(err)
			}
			if rg.OK() && rg.Hash == kept.Hash {
				t.Fatalf("the range to the kept head %+v still holds: %+v", kept, rg)
			}
		})
	}
}

// A second row at a position the chain holds, which takes the owner dropping
// the unique index, is in no chain and fails. The index is put back after.
func TestARowAtATakenPositionFails(t *testing.T) {
	p, id := chainedSession(t, 3)
	ctx := context.Background()
	t.Cleanup(func() {
		asOwner(t, p, []string{"events_no_delete"}, `DELETE FROM events WHERE session_id = '`+id+`' AND seq = 99`)
		if _, err := p.pool.Exec(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS events_chain_idx ON events (session_id, chain_pos) WHERE chain_pos IS NOT NULL`); err != nil {
			t.Errorf("put the index back: %v", err)
		}
	})
	asOwner(t, p, []string{"zz_events_chain"}, `DROP INDEX events_chain_idx`,
		`INSERT INTO events (id, session_id, tenant_id, seq, type, payload, actor, chain_pos, prev_hash, hash)
		 SELECT id || '-dup', session_id, tenant_id, 99, 'agent.forged', payload, actor, chain_pos, prev_hash, hash
		 FROM events WHERE session_id = '`+id+`' AND chain_pos = 3`)
	r, err := p.VerifyChain(ctx, id)
	if err != nil || r.OK() {
		t.Fatalf("verify = %+v, %v", r, err)
	}
	// Past the walk's page the row is not read at all; the count still shows it.
	defer func(n int) { chainPage = n }(chainPage)
	chainPage = 3
	if r, err := p.VerifyChain(ctx, id); err != nil || r.OK() || !strings.Contains(r.Reason, "position another holds") {
		t.Fatalf("verify with a page of 3 = %+v, %v", r, err)
	}
}

// A row written with the chain's trigger off has no chain, and is a failure,
// not a row "from before the chain".
func TestAnUnchainedRowAfterTheChainFails(t *testing.T) {
	p, id := chainedSession(t, 2)
	asOwner(t, p, []string{"zz_events_chain"},
		`INSERT INTO events (id, session_id, tenant_id, seq, type, payload, actor) VALUES ('`+id+`-x', '`+id+`', '`+p.tenant+`', 3, 'agent.message', '{}', 'agent')`)
	r, err := p.VerifyChain(context.Background(), id)
	if err != nil || r.OK() || r.Unchained != 1 || r.PreChain != 0 || !strings.Contains(r.Reason, "trigger off") {
		t.Fatalf("verify = %+v, %v", r, err)
	}
}

// Rows that were there before the chain are counted once, as they were, and
// never chained over; a later unchained row is still a failure.
func TestPreChainRowsAreMarkedNotChained(t *testing.T) {
	tenant := testID(t, "t-pre-")
	p := openStore(t, tenant)
	ctx := context.Background()
	id := testID(t, "sess-pre-")
	newSession(t, p, id, tenant)
	insert := func(seq int) string {
		return fmt.Sprintf(`INSERT INTO events (id, session_id, tenant_id, seq, type, payload, actor) VALUES ('%s-%d', '%s', '%s', %d, 'agent.message', '{}', 'agent')`,
			id, seq, id, tenant, seq)
	}
	// A session from before version 8: two rows with no chain, then chained ones.
	asOwner(t, p, []string{"zz_events_chain"}, insert(1), insert(2))
	for seq := int64(3); seq <= 4; seq++ {
		if err := p.Append(ev(id, seq, agent.EvAgentMessage, agent.Trusted, map[string]string{"text": "after"})); err != nil {
			t.Fatal(err)
		}
	}
	if r, _ := p.VerifyChain(ctx, id); r.OK() {
		t.Fatalf("unmarked rows with no chain passed: %+v", r)
	}
	if _, err := p.pool.Exec(ctx, `SELECT abhed_chain_mark_pre()`); err != nil {
		t.Fatal(err)
	}
	r, err := p.VerifyChain(ctx, id)
	if err != nil || !r.OK() || r.Rows != 2 || r.Unchained != 2 || r.PreChain != 2 {
		t.Fatalf("after marking: %+v, %v", r, err)
	}
	var chained int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND chain_pos IS NOT NULL`, id).Scan(&chained); err != nil || chained != 2 {
		t.Fatalf("the marking chained rows: %d, %v", chained, err)
	}
	// Marking again leaves the mark as it was, so it cannot cover a row written since.
	asOwner(t, p, []string{"zz_events_chain"}, insert(5))
	if _, err := p.pool.Exec(ctx, `SELECT abhed_chain_mark_pre()`); err != nil {
		t.Fatal(err)
	}
	if r, _ := p.VerifyChain(ctx, id); r.OK() || r.Unchained != 3 || r.PreChain != 2 {
		t.Fatalf("a row written after the chain began was accepted: %+v", r)
	}
}

// The runtime role reads the marks and cannot write them or remake them.
func TestRuntimeRoleCannotMarkPreChainRows(t *testing.T) {
	p := runtimeStore(t, "t-premark")
	ctx := context.Background()
	for what, q := range map[string]string{
		"insert a mark":    `INSERT INTO chain_pre (tbl, key, tenant_id, pre_rows, last_seq) VALUES ('events', 'x', 't-premark', 1, 1)`,
		"remake the marks": `SELECT abhed_chain_mark_pre()`,
		"delete the marks": `DELETE FROM chain_pre`,
	} {
		if _, err := p.pool.Exec(ctx, q); err == nil {
			t.Errorf("the runtime role could %s", what)
		}
	}
	if _, err := p.pool.Exec(ctx, `SELECT count(*) FROM chain_pre`); err != nil {
		t.Fatalf("the runtime role cannot read the marks: %v", err)
	}
}

// An export checks with nothing but the file, and a tamper with the file is found.
func TestExportedChainVerifiesOffline(t *testing.T) {
	p, id := chainedSession(t, 4)
	ctx := context.Background()
	var b bytes.Buffer
	if _, err := p.ExportChain(ctx, &b, id, false); err != nil {
		t.Fatal(err)
	}
	head, err := p.EventChain().Head(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	rep, _, err := chain.VerifyExport(bytes.NewReader(b.Bytes()), map[int64]string{head.Pos: head.Hash})
	if err != nil || !rep.OK() || rep.Links != 4 || rep.Head == nil || rep.Head.Hash != head.Hash {
		t.Fatalf("export = %+v, %v", rep, err)
	}
	// Payloads are sealed, so the edit is to a field the hash covers in the clear.
	lines := bytes.SplitAfter(b.Bytes(), []byte("\n"))
	lines[2] = bytes.Replace(lines[2], []byte("agent.message"), []byte("agent.messagf"), 1)
	edited := bytes.Join(lines, nil)
	if rep, _, _ := chain.VerifyExport(bytes.NewReader(edited), nil); rep.OK() || rep.FirstBad != 2 {
		t.Fatalf("an edited export = %+v", rep)
	}
	// A broken chain is not exported unless asked for, and then fails its own check.
	asOwner(t, p, []string{"events_no_update"}, `UPDATE events SET type = 'x' WHERE session_id = '`+id+`' AND seq = 2`)
	if _, err := p.ExportChain(ctx, &bytes.Buffer{}, id, false); err == nil {
		t.Fatal("a broken chain was exported")
	}
	b.Reset()
	if _, err := p.ExportChain(ctx, &b, id, true); err != nil {
		t.Fatal(err)
	}
	if rep, _, _ := chain.VerifyExport(bytes.NewReader(b.Bytes()), nil); rep.OK() || rep.FirstBad != 2 {
		t.Fatalf("an unverified export = %+v", rep)
	}
}

// The sandbox audit is one chain per tenant, read through the same API.
func TestAuditChainIsASource(t *testing.T) {
	tenant := fmt.Sprintf("cp-chain-%d", time.Now().UnixNano())
	p := runtimeStore(t, tenant)
	ctx := context.Background()
	q, err := p.Sandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := q.Append(ctx, controlplane.AuditEvent{At: time.Now().UTC(), Principal: "alice", Action: "exec",
			SandboxID: "sbx_00000000000000ab", Generation: int64(i + 1), Outcome: controlplane.OutcomeOK}); err != nil {
			t.Fatal(err)
		}
	}
	src := p.AuditChain()
	h, err := src.Head(ctx, tenant)
	if err != nil || h.Pos != 3 || len(h.Hash) != 64 {
		t.Fatalf("head = %+v, %v", h, err)
	}
	r, err := src.VerifyRange(ctx, tenant, 2, 3)
	if err != nil || !r.OK() || r.Hash != h.Hash || len(r.Before) != 64 || r.Before == chain.Genesis {
		t.Fatalf("range = %+v, %v", r, err)
	}
	if _, err := src.Head(ctx, "another-tenant"); err == nil {
		t.Fatal("another tenant's audit chain was read")
	}
	var links []chain.Link
	if err := src.(chain.Linker).Links(ctx, tenant, 1, 0, func(l chain.Link) bool { links = append(links, l); return true }); err != nil {
		t.Fatal(err)
	}
	if len(links) != 3 || links[2].Hash != h.Hash || chain.LinkHash(links[1].Prev, links[1].Row) != links[1].Hash {
		t.Fatalf("links %+v", links)
	}
	if err := src.(chain.Linker).Links(ctx, "another-tenant", 1, 0, func(chain.Link) bool { return true }); err == nil {
		t.Fatal("another tenant's audit links were read")
	}
}

// A runtime role that could add a mark to chain_pre could excuse an event
// written with the chain's trigger off: the server refuses to start as one.
func TestRuntimeRoleThatCanMarkPreChainRowsIsRefused(t *testing.T) {
	runtimeStore(t, "t-premark-grant") // provisions the runtime role
	owner := openStore(t, "t-premark-grant")
	rt := os.Getenv("ABHED_TEST_RUNTIME_DSN")
	rc, err := pgx.ParseConfig(rt)
	if err != nil {
		t.Fatal(err)
	}
	role := pgx.Identifier{rc.User}.Sanitize()
	ctx := context.Background()
	// Column grants too: a table-level check misses them.
	for _, g := range []struct{ grant, name string }{{"INSERT", "INSERT"}, {"UPDATE", "UPDATE"}, {"DELETE", "DELETE"},
		{"TRUNCATE", "TRUNCATE"}, {"INSERT (pre_rows)", "INSERT"}, {"UPDATE (pre_rows)", "UPDATE"}} {
		if _, err := owner.pool.Exec(ctx, `GRANT `+g.grant+` ON chain_pre TO `+role); err != nil {
			t.Fatal(err)
		}
		_, err := Open(ctx, DefaultConfig(rt))
		if _, rerr := owner.pool.Exec(ctx, `REVOKE `+g.grant+` ON chain_pre FROM `+role); rerr != nil {
			t.Fatal(rerr)
		}
		if err == nil || !strings.Contains(err.Error(), "holds "+g.name+" on chain_pre") {
			t.Errorf("a runtime role granted %s on chain_pre: %v", g.grant, err)
		}
	}
	if p, err := Open(ctx, DefaultConfig(rt)); err != nil {
		t.Fatalf("the runtime role, as provisioned, is refused: %v", err)
	} else {
		p.Close()
	}
}

// The trigger sets commit_tx, whatever the runtime role sends, so no event
// can be kept out of the commit-ordered scan by a value of the writer's.
func TestRuntimeRoleCannotSetCommitTx(t *testing.T) {
	p := runtimeStore(t, "t-commit-tx")
	ctx := context.Background()
	id := testID(t, "sess-ctx-")
	newSession(t, p, id, "t-commit-tx")
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The writing transaction, shifted as every commit_tx is (schema version
	// 17; the shift is 0 unless the database was restored from elsewhere).
	var mine string
	if err := tx.QueryRow(ctx, `SELECT (pg_current_xact_id()::text::numeric + shift)::text FROM public.commit_shift`).Scan(&mine); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events (id, session_id, tenant_id, seq, type, payload, actor, commit_tx)
		VALUES ($1, $2, 't-commit-tx', 1, 'agent.message', '{}', 'agent', '18446744073709551615'::xid8)`, id+"-1", id); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := tx.QueryRow(ctx, `SELECT commit_tx::text FROM events WHERE id = $1`, id+"-1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != mine {
		t.Fatalf("commit_tx stored as %s, want the writing transaction %s", stored, mine)
	}
}
