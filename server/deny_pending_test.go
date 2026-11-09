package server_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/server/servertest"
)

// DenyPending decides only its owner's waiting request, named by id: the
// asked-for write never runs, the run goes on, and a second answer finds
// nothing to decide.
func TestDenyPendingDecidesTheOwnersRequestOnce(t *testing.T) {
	work := t.TempDir()
	mdl := &servertest.Scripted{Reply: func(_, _ string, fromTool bool) servertest.Turn {
		if !fromTool {
			return servertest.Turn{Tool: "write", Args: map[string]any{"path": filepath.Join(work, "m.txt"), "content": "x"}}
		}
		return servertest.Turn{Text: "done"}
	}}
	s := servertest.Proxy(t, servertest.WithShellAndWrite, func(o *server.Options) { o.Workspace = work; o.Adapter = mdl })
	ended := make(chan string, 1)
	id, err := s.StartSession(context.Background(), server.StartSpec{Prompt: "go", User: "alice", Tenant: "t1",
		OnEnd: func(reason string, _ error) { ended <- reason }})
	if err != nil {
		t.Fatal(err)
	}
	var ask server.PendingAsk
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if asks := s.PendingAsks(func(string, string) bool { return true }); len(asks) == 1 {
			ask = asks[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no ask")
		}
	}
	ctx := context.Background()
	for _, c := range []struct{ tenant, owner, req string }{
		{"t1", "bob", ask.RequestID}, {"t2", "alice", ask.RequestID}, {"t1", "alice", "req-other"}, {"t1", "alice", ""},
	} {
		if s.DenyPending(ctx, c.tenant, c.owner, id, c.req) {
			t.Fatalf("denied as %+v", c)
		}
	}
	if !s.DenyPending(ctx, "t1", "alice", id, ask.RequestID) {
		t.Fatal("the owner's request was not denied")
	}
	select {
	case r := <-ended:
		if r != "completed" {
			t.Fatalf("the run ended %s", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not go on after the denial")
	}
	if s.DenyPending(ctx, "t1", "alice", id, ask.RequestID) {
		t.Fatal("a decided request was decided again")
	}
	if _, err := os.Stat(filepath.Join(work, "m.txt")); !os.IsNotExist(err) {
		t.Fatalf("the denied write ran: %v", err)
	}
}
