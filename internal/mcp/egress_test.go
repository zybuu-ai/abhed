package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Under the allowlist an allowed HTTP server connects and each call to it is
// recorded; another is refused before it is reached.
func TestHTTPServerUnderTheAllowlist(t *testing.T) {
	srv := modernServer(t)
	defer srv.Close()
	port := int(netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://")).Port())
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{
		{Host: "127.0.0.1", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	guard := egress.NewGuard(egress.GuardOptions{Policy: pol})
	defer guard.Close()
	defer egress.Install(guard)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := NewGateway()
	defer g.Close()
	errs := g.Connect(ctx, []ServerConfig{{Name: "ok", URL: srv.URL, Enabled: true},
		{Name: "elsewhere", URL: fmt.Sprintf("http://localhost:%d", port), Enabled: true}})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "refused") {
		t.Fatalf("errors %v", errs)
	}
	var mu sync.Mutex
	var got []map[string]any
	cctx := egress.WithCaller(ctx, egress.Caller{Session: "s1", CallID: "call-m",
		Record: func(_ string, m map[string]any) error { mu.Lock(); got = append(got, m); mu.Unlock(); return nil }})
	ts := g.Tools()
	if len(ts) != 1 {
		t.Fatalf("%d tools", len(ts))
	}
	if r := ts[0].Run(cctx, nil, []byte(`{}`)); r.IsError {
		t.Fatalf("call: %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0]["decision"] != "allow" || got[0]["kind"] != egress.KindMCP || got[0]["call_id"] != "call-m" ||
		got[0]["session"] != "s1" || got[0]["method"] != "POST" {
		t.Fatalf("records: %v", got)
	}
}

// A stdio server the allowlist needs confined is not started unconfined;
// one that can be is started through the launcher.
func TestStdioServerConfinedUnderTheAllowlist(t *testing.T) {
	ctx := context.Background()
	g := NewGateway()
	g.MustConfine = true
	errs := g.Connect(ctx, []ServerConfig{{Name: "s", Command: "true", Enabled: true}})
	if len(errs) != 1 || !errors.Is(errs[0], errUnconfined) {
		t.Fatalf("an unconfinable server: %v", errs)
	}
	var argv []string
	g.Confine = func(_ context.Context, name string, a, _ []string, _ func(string, map[string]any) error) (*exec.Cmd, error) {
		argv = append([]string{name}, a...)
		return nil, errors.New("no proxy here")
	}
	errs = g.Connect(ctx, []ServerConfig{{Name: "s", Command: "srv", Args: []string{"-x"}, Enabled: true}})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "no proxy here") || strings.Join(argv, " ") != "s srv -x" {
		t.Fatalf("%v %v", errs, argv)
	}
}

// A stdio server's decisions go to the session whose call is in flight, or
// with none the latest; calls of two sessions at once are not attributed.
func TestStdioServerDecisionsGoToTheCaller(t *testing.T) {
	var s serverCallers
	var mu sync.Mutex
	recs := map[string][]map[string]any{}
	caller := func(session, call string) egress.Caller {
		return egress.Caller{Session: session, CallID: call, Record: func(_ string, m map[string]any) error {
			mu.Lock()
			recs[session] = append(recs[session], m)
			mu.Unlock()
			return nil
		}}
	}
	rec := s.recordFor("srv")
	_ = rec(egress.EventName, map[string]any{"host": "a"}) // before any call: logged only
	endA := s.begin("srv", caller("A", "a1"))
	_ = rec(egress.EventName, map[string]any{"host": "b", "call_id": "mcp/srv"})
	endB := s.begin("srv", caller("B", "b1"))
	_ = rec(egress.EventName, map[string]any{"host": "c"}) // two sessions: logged only
	endA()
	endB()
	_ = rec(egress.EventName, map[string]any{"host": "d"}) // none in flight: the latest, B
	if len(recs["A"]) != 1 || recs["A"][0]["host"] != "b" || recs["A"][0]["call_id"] != "a1" || recs["A"][0]["mcp_server"] != "srv" {
		t.Fatalf("A: %v", recs["A"])
	}
	if len(recs["B"]) != 1 || recs["B"][0]["host"] != "d" || recs["B"][0]["call_id"] != "" || recs["B"][0]["session"] != "B" {
		t.Fatalf("B: %v", recs["B"])
	}
	// Two calls of one session: its record, naming neither call.
	end1, end2 := s.begin("srv", caller("A", "a2")), s.begin("srv", caller("A", "a3"))
	_ = rec(egress.EventName, map[string]any{"host": "e"})
	end1()
	end2()
	if len(recs["A"]) != 2 || recs["A"][1]["host"] != "e" || recs["A"][1]["call_id"] != "" {
		t.Fatalf("A: %v", recs["A"])
	}
}
