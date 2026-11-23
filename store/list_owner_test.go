package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zybuu-ai/abhed/internal/agent"
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

// A listed row says when its session last recorded an event, and its start
// before it has recorded any.
func TestListSessionsCarriesLastActivity(t *testing.T) {
	tenant := fmt.Sprintf("t-upd-%d", time.Now().UnixNano())
	p := runtimeStore(t, tenant)
	ctx := context.Background()
	started := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	quiet, busy := tenant+"-quiet", tenant+"-busy"
	for _, id := range []string{quiet, busy} {
		if err := p.CreateSession(ctx, SessionRecord{ID: id, Tenant: tenant, User: "local:ana", StartedAt: started}); err != nil {
			t.Fatal(err)
		}
	}
	rec := agent.NewRecorder(p, busy, "")
	if _, err := rec.Record(agent.EvSessionStarted, agent.ActorSystem, agent.Trusted, nil); err != nil {
		t.Fatal(err)
	}
	ev, err := rec.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, map[string]string{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	// Looking after the session afterwards is not activity.
	time.Sleep(5 * time.Millisecond)
	for _, after := range []struct {
		t agent.EventType
		a agent.Actor
		p any
	}{
		{agent.EvActionRequested, agent.ActorUser, map[string]string{"tool": "bash"}},
		{agent.EvSessionRenamed, agent.ActorUser, agent.SessionRenamed{Title: "Renamed"}},
		{agent.EvModelSwitched, agent.ActorUser, agent.ModelSwitched{Model: "m2"}},
	} {
		if _, err := rec.Record(after.t, after.a, agent.Trusted, after.p); err != nil {
			t.Fatal(err)
		}
	}
	list, err := p.ListSessionsOwnedBy(ctx, "local:ana", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]time.Time{}
	for _, r := range list {
		got[r.ID] = r.UpdatedAt
	}
	if !got[quiet].Equal(started) {
		t.Errorf("a session with no events is listed as updated at %v, want its start %v", got[quiet], started)
	}
	if d := got[busy].Sub(ev.CreatedAt); d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("a session is listed as updated at %v, its conversation last went on at %v", got[busy], ev.CreatedAt)
	}
}

// A name the command line gives becomes the row's title, as a rename on the
// server does; a name the title rules refuse leaves the title as it was.
func TestSessionNamedSetsTheTitle(t *testing.T) {
	tenant := fmt.Sprintf("t-named-%d", time.Now().UnixNano())
	p := runtimeStore(t, tenant)
	ctx := context.Background()
	id := tenant + "-s"
	if err := p.CreateSession(ctx, SessionRecord{ID: id, Tenant: tenant, User: "local:ana", StartedAt: time.Now(), Prompt: "draft the notes"}); err != nil {
		t.Fatal(err)
	}
	rec := agent.NewRecorder(p, id, "")
	title := func() string {
		r, err := p.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r.Title
	}
	name := func(n string) {
		if _, err := rec.Record(agent.EvSessionNamed, agent.ActorUser, agent.Trusted, agent.SessionNamed{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	name("  Release notes  ")
	if got := title(); got != "Release notes" {
		t.Fatalf("after /rename the title is %q", got)
	}
	for _, refused := range []string{"notes\u202edm", "two\nlines", strings.Repeat("x", 121)} {
		name(refused)
		if got := title(); got != "Release notes" {
			t.Fatalf("a name the rules refuse (%q) became the title %q", refused, got)
		}
	}
	list, err := p.ListSessionsOwnedBy(ctx, "local:ana", 10)
	if err != nil || len(list) != 1 || list[0].Title != "Release notes" {
		t.Fatalf("the list shows %+v, %v", list, err)
	}
}

// GuestLeftovers lists only console-guest sessions no live guest can hold:
// one held by this node's earlier run, an open one whose holder stopped
// heartbeating, an ended one older than a guest's hours. Another node's
// live session, its recent ended one, this run's own and other owners' are
// left out, as is anything deleted.
func TestGuestLeftovers(t *testing.T) {
	tenant := fmt.Sprintf("t-guests-%d", time.Now().UnixNano())
	p := runtimeStore(t, tenant)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenant); err != nil {
		t.Fatal(err)
	}
	row := func(name, user, node, set string) string {
		id := tenant + "-" + name
		if err := p.CreateSession(ctx, SessionRecord{ID: id, Tenant: tenant, User: user, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, "UPDATE sessions SET node_id = $2, node_seen_at = now() WHERE id = $1", id, node); err != nil {
			t.Fatal(err)
		}
		if set != "" {
			if _, err := conn.Exec(ctx, "UPDATE sessions SET "+set[2:]+" WHERE id = $1", id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	want := []string{
		row("earlier-run", "guest:a", "node-b#before", ", ended_at = now()"),
		row("stale-open", "guest:b", "node-c#1", ", node_seen_at = now() - interval '10 minutes'"),
		row("old-ended", "guest:c", "node-c#1", ", ended_at = now(), started_at = now() - interval '9 hours'"),
	}
	sort.Strings(want)
	// Open with no heartbeat (released after a run that wrote no end, or
	// parked): possibly a live guest's, until a guest's hours have passed.
	row("open-released-recent", "guest:g", "node-a#1", ", node_seen_at = NULL, started_at = now() - interval '10 minutes'")
	want = append(want, row("open-released-old", "guest:h", "node-a#1", ", node_seen_at = NULL, started_at = now() - interval '9 hours'"))
	sort.Strings(want)
	row("live-elsewhere", "guest:d", "node-a#1", "")
	row("recent-ended-elsewhere", "guest:d", "node-a#1", ", ended_at = now()")
	row("this-run", "guest:e", "node-b#now", ", node_seen_at = now() - interval '10 minutes'")
	row("not-a-guest", "local:ana", "node-b#before", ", ended_at = now()")
	deleted := row("deleted", "guest:f", "node-b#before", ", ended_at = now()")
	if err := p.DeleteSession(deleted); err != nil {
		t.Fatal(err)
	}
	got, err := p.GuestLeftovers(ctx, "guest:", "node-b#now", true, 2*time.Minute, 8*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		if r.Tenant != tenant || !strings.HasPrefix(r.User, "guest:") {
			t.Errorf("row %+v", r)
		}
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("leftovers %v, want %v", ids, want)
	}

	// Without the earlier-run case, the earlier run's ended session stays.
	got, err = p.GuestLeftovers(ctx, "guest:", "node-b#now", false, 2*time.Minute, 8*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if strings.HasSuffix(r.ID, "-earlier-run") {
			t.Fatalf("the earlier run's session was listed with that case off: %s", r.ID)
		}
	}

	// node-a#1 heartbeated an open session just now (live-elsewhere):
	// another live run under node-a. node-b#before's rows are ended, so
	// however fresh their last beat, it is not a live run.
	if live, err := p.SameNodeLive(ctx, "node-a#2", time.Minute); err != nil || !live {
		t.Fatalf("SameNodeLive(node-a#2) = %v, %v", live, err)
	}
	if live, err := p.SameNodeLive(ctx, "node-a#1", time.Minute); err != nil || live {
		t.Fatalf("SameNodeLive counted the run itself: %v, %v", live, err)
	}
	if live, err := p.SameNodeLive(ctx, "node-b#now", time.Minute); err != nil || live {
		t.Fatalf("SameNodeLive took an earlier run for a live one: %v, %v", live, err)
	}
}
