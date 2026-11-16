package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/server/servertest"
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
