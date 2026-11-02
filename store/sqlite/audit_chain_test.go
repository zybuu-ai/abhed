package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/store/chain"
)

// The control plane's own record, the reconciler's events among them, is
// chained; an edit by whoever can write the file breaks it at that row.
func TestAuditChainShowsAnEditedRow(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for i, o := range []string{"requested", "ok", "ok", "failed"} {
		if err := s.Append(ctx, controlplane.AuditEvent{At: time.Now(), Principal: controlplane.SystemPrincipal,
			Action: "reconcile", SandboxID: "sb1", Generation: int64(i), Outcome: o, Detail: map[string]string{"k": "v"}}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.VerifyAuditChain(ctx)
	if err != nil || !r.OK() || r.Rows != 4 || len(r.Head) != 64 || r.Unchained != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	// What only the file's owner can do: the append-only trigger dropped.
	if _, err := s.w.Exec(`DROP TRIGGER sandbox_audit_no_update;
		UPDATE sandbox_audit SET principal = 'mallory' WHERE chain_pos = 3`); err != nil {
		t.Fatal(err)
	}
	if r, err := s.VerifyAuditChain(ctx); err != nil || r.FirstBad != 3 {
		t.Fatalf("edit not found at 3: %+v %v", r, err)
	}
	if _, err := s.w.Exec(`UPDATE sandbox_audit SET principal = 'system:reconciler' WHERE chain_pos = 3;
		UPDATE sandbox_audit SET chain_pos = chain_pos + 10 WHERE chain_pos = 2`); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.VerifyAuditChain(ctx); r.OK() {
		t.Fatal("a row out of place verified")
	}
}

// Rows from before the chain are counted, not failed.
func TestAuditChainCountsUnchainedRows(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	if _, err := s.w.Exec(`INSERT INTO sandbox_audit (at, principal, action, sandbox_id, generation, outcome)
		VALUES ('2026-01-01T00:00:00Z', 'a', 'create', 'sb', 1, 'ok')`); err != nil {
		t.Fatal(err)
	}
	_ = s.Append(ctx, controlplane.AuditEvent{At: time.Now(), Principal: "a", Action: "delete", SandboxID: "sb", Outcome: "ok"})
	if r, err := s.VerifyAuditChain(ctx); err != nil || !r.OK() || r.Unchained != 1 || r.Rows != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}

// The control plane's record answers the chain API, and a head kept before
// an edit no longer matches.
func TestAuditChainIsASource(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for i := range 3 {
		if err := s.Append(ctx, controlplane.AuditEvent{At: time.Now(), Principal: "a", Action: "create", SandboxID: "sb",
			Generation: int64(i), Outcome: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	src := s.AuditChain()
	h, err := src.Head(ctx, "")
	if err != nil || h.Pos != 3 || len(h.Hash) != 64 {
		t.Fatalf("head = %+v, %v", h, err)
	}
	if r, err := src.VerifyRange(ctx, "", 2, 3); err != nil || !r.OK() || r.Hash != h.Hash {
		t.Fatalf("range = %+v, %v", r, err)
	}
	if _, err := s.w.Exec(`DROP TRIGGER sandbox_audit_no_update; UPDATE sandbox_audit SET outcome = 'failed' WHERE chain_pos = 2`); err != nil {
		t.Fatal(err)
	}
	if r, _ := src.VerifyRange(ctx, "", 1, 3); r.OK() || r.FirstBad != 2 {
		t.Fatalf("after an edit = %+v", r)
	}
}

// The record hands out its links as hashed, so an export checks them with
// nothing but the links.
func TestAuditChainHandsOutItsLinks(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for i := range 4 {
		if err := s.Append(ctx, controlplane.AuditEvent{At: time.Now(), Principal: "a", Action: "create", SandboxID: "sb",
			Generation: int64(i), Outcome: "ok", Detail: map[string]string{"n": "v"}}); err != nil {
			t.Fatal(err)
		}
	}
	src := s.AuditChain().(chain.Linker)
	var got []chain.Link
	if err := src.Links(ctx, "", 2, 0, func(l chain.Link) bool { got = append(got, l); return true }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Pos != 2 {
		t.Fatalf("links %+v", got)
	}
	for _, l := range got {
		if chain.LinkHash(l.Prev, l.Row) != l.Hash {
			t.Fatalf("link %d does not hash as stored", l.Pos)
		}
	}
	n := 0
	_ = src.Links(ctx, "", 1, 0, func(chain.Link) bool { n++; return n < 2 })
	if n != 2 {
		t.Fatalf("stopping after two read %d", n)
	}
}

// A row with no chain written after the chain began, as only one who bypassed
// it could write, fails the check, as on Postgres.
func TestAuditChainFailsAnUnchainedRowAfterItBegan(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	_ = s.Append(ctx, controlplane.AuditEvent{At: time.Now(), Principal: "a", Action: "create", SandboxID: "sb", Outcome: "ok"})
	if _, err := s.w.Exec(`INSERT INTO sandbox_audit (at, principal, action, sandbox_id, generation, outcome)
		VALUES ('2026-01-01T00:00:00Z', 'a', 'delete', 'sb', 1, 'ok')`); err != nil {
		t.Fatal(err)
	}
	if r, err := s.VerifyAuditChain(ctx); err != nil || r.OK() || r.Unchained != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}
