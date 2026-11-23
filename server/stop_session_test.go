package server_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/server/servertest"
	"github.com/zybuu-ai/abhed/store/local"
)

// hangingAdapter answers nothing until the run is cancelled.
type hangingAdapter struct{ servertest.StubAdapter }

func (hangingAdapter) Complete(ctx context.Context, _ model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

// StopSession stops only a session its owner runs in its tenant: another
// person, another tenant or an unknown id is told no and changes nothing.
func TestStopSessionStopsOnlyTheOwnersRun(t *testing.T) {
	s := servertest.Proxy(t, func(o *server.Options) { o.Adapter = hangingAdapter{} })
	ended := make(chan string, 1)
	id, err := s.StartSession(context.Background(), server.StartSpec{Prompt: "wait", User: "alice", Tenant: "t1",
		OnEnd: func(reason string, _ error) { ended <- reason }})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tenant, owner, id string }{
		{"t1", "bob", id}, {"t2", "alice", id}, {"t1", "", id}, {"t1", "alice", "sess_nope"},
	} {
		if s.StopSession(c.tenant, c.owner, c.id) {
			t.Fatalf("stopped as %+v", c)
		}
	}
	select {
	case r := <-ended:
		t.Fatalf("the run ended (%s) though nobody who owns it stopped it", r)
	case <-time.After(100 * time.Millisecond):
	}
	if !s.StopSession("t1", "alice", id) {
		t.Fatal("the owner could not stop the run")
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end after StopSession")
	}
}

// A console guest's run that is still going when the guest signs out or
// expires is cancelled, not deleted underneath it: the end hook removes by
// owner alone, with no tenant, and the run is in the deployment's tenant.
func TestRemoveOwnerSessionsCancelsARunningGuestRun(t *testing.T) {
	s := servertest.Proxy(t, func(o *server.Options) { o.Adapter = hangingAdapter{} })
	ended := make(chan string, 1)
	if _, err := s.StartSession(context.Background(), server.StartSpec{Prompt: "wait", User: "guest:abc", Tenant: "default",
		OnEnd: func(reason string, _ error) { ended <- reason }}); err != nil {
		t.Fatal(err)
	}
	if n := s.RemoveOwnerSessions(context.Background(), "", "guest:abc"); n != 1 {
		t.Fatalf("removed %d sessions, want 1", n)
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the guest's run kept going after its sessions were removed")
	}
}

// A guest lives only in the process's memory, so the sessions of guests from
// before a restart are removed at start; no one else's are touched.
func TestGuestLeftoversAreRemovedAtStart(t *testing.T) {
	st, err := local.Open(local.Options{Dir: filepath.Join(t.TempDir(), "records")})
	if err != nil {
		t.Fatal(err)
	}
	s := servertest.Proxy(t, func(o *server.Options) { o.Store = st })
	ended := make(chan string, 3)
	for _, u := range []string{"guest:old", "guest:older", "alice"} {
		if _, err := s.StartSession(context.Background(), server.StartSpec{Prompt: "hi", User: u, Tenant: "default",
			OnEnd: func(reason string, _ error) { ended <- reason }}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		select {
		case <-ended:
		case <-time.After(10 * time.Second):
			t.Fatal("the runs did not end")
		}
	}
	// A later sweep takes only sessions older than a guest's hours, so a
	// guest of this process that is still signed in keeps its ended ones.
	if n := s.RemoveGuestLeftovers(context.Background(), 8*time.Hour, false); n != 0 {
		t.Fatalf("an hourly sweep removed %d recent guest sessions", n)
	}
	// The process holds its sessions, so its own sweep leaves them; the next
	// process on the store, as after a restart, removes the guests'.
	if n := s.RemoveGuestLeftovers(context.Background(), 8*time.Hour, true); n != 0 {
		t.Fatalf("a sweep removed %d sessions this process still holds", n)
	}
	restarted := servertest.Proxy(t, func(o *server.Options) { o.Store = st })
	if n := restarted.RemoveGuestLeftovers(context.Background(), 8*time.Hour, true); n != 2 {
		t.Fatalf("removed %d, want the two guests' sessions", n)
	}
	recs, err := st.ListSessions(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].User != "alice" {
		t.Fatalf("left %+v", recs)
	}
}
