package mcp_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/mcp"
)

// A server connected within a request's context, as an admin adds one at run
// time, lives as the gateway does: it still answers once that context has
// ended, and a restart brings it back.
func TestServerOutlivesTheContextItWasAddedIn(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	g := mcp.NewGateway()
	defer g.Close()
	// The serve-time Connect sets the gateway's life; this one has none to add.
	if errs := g.Connect(context.Background(), nil); len(errs) > 0 {
		t.Fatal(errs)
	}
	req, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if errs := g.Connect(req, []mcp.ServerConfig{{Name: "late", Command: exe, Env: []string{"ABHED_MCP_FAKE_SERVER=1"}, Enabled: true}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	cancel() // the request has been answered
	call := func(when string) {
		t.Helper()
		ts := g.Tools()
		if len(ts) != 1 {
			t.Fatalf("%s: %d tools", when, len(ts))
		}
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if r := ts[0].Run(ctx, nil, []byte(`{}`)); r.IsError {
			t.Fatalf("%s: %+v", when, r)
		}
	}
	time.Sleep(200 * time.Millisecond) // a process tied to the request would be gone by now
	call("after the request")
	if err := g.Restart(context.Background(), "late"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	call("after a restart")

	// A first Connect made with a request's context does not make it the gateway's life either.
	g2 := mcp.NewGateway()
	defer g2.Close()
	req2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	if errs := g2.Connect(req2, []mcp.ServerConfig{{Name: "first", Command: exe, Env: []string{"ABHED_MCP_FAKE_SERVER=1"}, Enabled: true}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	cancel2()
	if err := g2.Restart(context.Background(), "first"); err != nil {
		t.Fatalf("restart after the first Connect's context ended: %v", err)
	}
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	if ts := g2.Tools(); len(ts) != 1 || ts[0].Run(ctx, nil, []byte(`{}`)).IsError {
		t.Fatal("the restarted server does not answer")
	}
}
