package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A restart reconnects a server, and tools handed out before it reach the
// new connection; a server that failed is listed with why; an unknown name
// is refused.
func TestGatewayRestartAndServers(t *testing.T) {
	srv := modernServer(t)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := NewGateway()
	defer g.Close()
	errs := g.Connect(ctx, []ServerConfig{{Name: "ok", URL: srv.URL, Enabled: true}, {Name: "bad", URL: "http://127.0.0.1:9", Enabled: true}})
	if len(errs) != 1 {
		t.Fatalf("errors %v", errs)
	}
	ts := g.Tools()
	if len(ts) != 1 {
		t.Fatalf("%d tools", len(ts))
	}
	st := g.Servers()
	if len(st) != 2 || st[0].Name != "bad" || st[0].Connected || st[0].Err == nil || !st[1].Connected || st[1].Tools[0] != "search" {
		t.Fatalf("%+v", st)
	}
	if err := g.Restart(ctx, "ok"); err != nil {
		t.Fatal(err)
	}
	if r := ts[0].Run(ctx, nil, []byte(`{}`)); r.IsError || !strings.Contains(r.Content, "three results") {
		t.Fatalf("after the restart: %+v", r)
	}
	if err := g.Restart(ctx, "nope"); err == nil {
		t.Fatal("restarted an unknown server")
	}
	g.Close()
	if r := ts[0].Run(ctx, nil, []byte(`{}`)); !r.IsError || !strings.Contains(r.Content, "not connected") {
		t.Fatalf("after close: %+v", r)
	}
}
