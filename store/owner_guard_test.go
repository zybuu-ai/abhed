package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
)

// As the runtime role, a session's owner changes only to its unclaimed form,
// which is recorded; its id, tenant and parent never change.
func TestRuntimeCannotMoveASessionsOwner(t *testing.T) {
	p := runtimeStore(t, "default")
	ctx := context.Background()
	id := testID(t, "sess-owner-")
	if err := p.CreateSession(ctx, SessionRecord{ID: id, Tenant: "default", User: "local:alice", Workspace: "/w",
		Model: "m", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE sessions SET user_id = 'local:mallory' WHERE id = $1`,
		`UPDATE sessions SET tenant_id = 'other' WHERE id = $1`,
		`UPDATE sessions SET parent_id = id WHERE id = $1`,
		`INSERT INTO owner_changes (tenant_id, session_id, from_user, to_user, by_role) VALUES ('default', $1, 'a', 'b', 'me')`,
	} {
		if _, err := p.pool.Exec(ctx, q, id); err == nil {
			t.Errorf("the runtime role ran %q", q)
		}
	}
	if _, err := p.pool.Exec(ctx, `UPDATE sessions SET user_id = 'unclaimed:' || user_id WHERE id = $1`, id); err != nil {
		t.Fatalf("unclaiming a removed account's session was refused: %v", err)
	}
	var from, to, by string
	if err := p.pool.QueryRow(ctx, `SELECT from_user, to_user, by_role FROM owner_changes WHERE session_id = $1`, id).
		Scan(&from, &to, &by); err != nil {
		t.Fatalf("the owner change was not recorded: %v", err)
	}
	if from != "local:alice" || to != "unclaimed:local:alice" || by == "" {
		t.Fatalf("recorded %s → %s by %q", from, to, by)
	}
}

// An account's groups change only with a row in account_changes, which the
// runtime role cannot write itself.
func TestAccountGroupChangesAreRecorded(t *testing.T) {
	p := runtimeStore(t, "default")
	ctx := context.Background()
	name := strings.ToLower(testID(t, "acct"))
	if err := p.Create(ctx, &auth.User{Username: name, Hash: "x", Tenant: "default", Groups: []string{"dev"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(ctx, &auth.User{Username: name, Hash: "x", Tenant: "default", Groups: []string{"dev", "admin"}}); err != nil {
		t.Fatal(err)
	}
	var old, now, by string
	if err := p.pool.QueryRow(ctx, `SELECT old_groups, new_groups, by_role FROM account_changes
		WHERE username = $1 AND change = 'changed'`, name).Scan(&old, &now, &by); err != nil {
		t.Fatalf("the group change was not recorded: %v", err)
	}
	if old != "dev" || now != "dev,admin" || by == "" {
		t.Fatalf("recorded %q → %q by %q", old, now, by)
	}
	if _, err := p.pool.Exec(ctx, `INSERT INTO account_changes (username, change, by_role) VALUES ($1, 'changed', 'me')`, name); err == nil {
		t.Fatal("the runtime role wrote account_changes itself")
	}
	if _, err := p.pool.Exec(ctx, `DELETE FROM account_changes WHERE username = $1`, name); err == nil {
		t.Fatal("the runtime role removed an account change")
	}
}
