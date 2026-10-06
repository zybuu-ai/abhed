package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

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
