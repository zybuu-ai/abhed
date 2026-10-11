package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A session can name as its parent only a session of its own tenant: one
// naming another tenant's session is refused exactly as one naming a session
// that does not exist, so the refusal confirms nothing about the other tenant.
func TestParentIsFoundInTheSessionsOwnTenant(t *testing.T) {
	a, ctx := approvalStore(t, "tenant-a-parent")
	b, _ := approvalStore(t, "tenant-b-parent")
	parent := SessionRecord{ID: fmt.Sprintf("s-pa-%d", time.Now().UnixNano()),
		Tenant: "tenant-a-parent", User: "ann", Workspace: "/w", Model: "m", StartedAt: time.Now()}
	if err := a.CreateSession(ctx, parent); err != nil {
		t.Fatalf("parent: %v", err)
	}
	child := func(id, parentID string) error {
		return b.CreateSession(ctx, SessionRecord{ID: id, Tenant: "tenant-b-parent", User: "bob", Workspace: "/w",
			Model: "m", StartedAt: time.Now(), ParentID: parentID})
	}
	errOther := child("s-pb-1-"+parent.ID, parent.ID)
	errNone := child("s-pb-2-"+parent.ID, "s-never-"+parent.ID)
	if errOther == nil {
		t.Fatal("a session named another tenant's session as its parent")
	}
	if errNone == nil {
		t.Fatal("a session named a parent that does not exist")
	}
	norm := func(err error) string { return strings.ReplaceAll(err.Error(), parent.ID, "X") }
	if got, want := strings.ReplaceAll(norm(errOther), "s-pb-1-", ""), strings.ReplaceAll(strings.ReplaceAll(norm(errNone), "s-never-", ""), "s-pb-2-", ""); got != want {
		t.Fatalf("the refusals differ, which tells the tenants apart:\n%s\n%s", got, want)
	}
	// The same tenant's parent is accepted.
	if err := a.CreateSession(ctx, SessionRecord{ID: "s-pa-child-" + parent.ID, Tenant: "tenant-a-parent", User: "ann",
		Workspace: "/w", Model: "m", StartedAt: time.Now(), ParentID: parent.ID}); err != nil {
		t.Fatalf("a child in the parent's own tenant: %v", err)
	}
}
