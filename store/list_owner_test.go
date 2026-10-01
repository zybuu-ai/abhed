package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// An owner whose sessions are all older than the tenant's newest 200 still
// lists them: the owner is filtered in the query, not after a bounded read.
func TestListSessionsOwnedByReachesOlderSessions(t *testing.T) {
	tenant := fmt.Sprintf("t-list-%d", time.Now().UnixNano())
	p := runtimeStore(t, tenant)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 2; i++ {
		if err := p.CreateSession(ctx, SessionRecord{ID: fmt.Sprintf("%s-mine-%d", tenant, i), Tenant: tenant,
			User: "local:old", StartedAt: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 205; i++ {
		if err := p.CreateSession(ctx, SessionRecord{ID: fmt.Sprintf("%s-busy-%d", tenant, i), Tenant: tenant,
			User: "local:busy", StartedAt: base.Add(time.Minute + time.Duration(i)*time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := p.ListSessions(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.User == "local:old" {
			t.Fatal("setup: an old session is among the tenant's newest 200")
		}
	}
	mine, err := p.ListSessionsOwnedBy(ctx, "local:old", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 {
		t.Fatalf("listed %d of the owner's 2 sessions", len(mine))
	}
	for _, r := range mine {
		if r.User != "local:old" {
			t.Fatalf("listed another owner's session %s (%s)", r.ID, r.User)
		}
	}
}
