package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

// A non-HTTP client's connection, such as ssh_connect's, is judged as a
// tunnel: an allowed host connects and is recorded with its kind, one no rule
// allows is refused before any connection, and an internal address no rule
// names is refused at the dial.
func TestDialIsJudgedAsATunnel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	g := ownGuard(t, Config{Rules: []Rule{
		{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
		{Host: "internal.test", Ports: []int{port}, Decision: "allow"},
		{Host: "narrow.test", Ports: []int{port}, Decision: "allow", Methods: []string{"GET"}},
	}}, GuardOptions{})
	var p payloads
	ctx := WithGuard(WithCaller(context.Background(), Caller{Session: "s1", CallID: "c1", Record: p.rec}), g)
	conn, err := Dial(ctx, KindSSH, "tcp", fmt.Sprintf("allowed.test:%d", port))
	if err != nil {
		t.Fatalf("allowed: %v", err)
	}
	_ = conn.Close()
	for _, host := range []string{"denied.test", "internal.test", "narrow.test"} {
		_, err := Dial(ctx, KindSSH, "tcp", fmt.Sprintf("%s:%d", host, port))
		var de *DeniedError
		if !errors.As(err, &de) {
			t.Errorf("%s: %v", host, err)
		}
	}
	got := p.all()
	if len(got) != 4 || got[0]["decision"] != "allow" || got[0]["kind"] != KindSSH || got[0]["ip"] != "127.0.0.1" {
		t.Fatalf("records: %v", got)
	}
	for _, e := range got[1:] {
		if e["decision"] != "deny" || e["kind"] != KindSSH {
			t.Errorf("record: %v", e)
		}
	}
	// Outside the allowlist it dials as the client would.
	conn, err = Dial(WithGuard(context.Background(), Unguarded), KindSSH, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("unguarded: %v", err)
	}
	_ = conn.Close()
}

// Allow judges a connection a client makes itself, such as git's push.
func TestAllowJudgesByHostAndPort(t *testing.T) {
	g := ownGuard(t, Config{Rules: []Rule{{Host: "forge.test", Ports: []int{443}, Decision: "allow"}}}, GuardOptions{})
	if err := g.Allow(context.Background(), KindForge, "forge.test", 443); err != nil {
		t.Fatal(err)
	}
	var de *DeniedError
	if err := g.Allow(context.Background(), KindForge, "other.test", 443); !errors.As(err, &de) {
		t.Fatalf("other host: %v", err)
	}
	var none *Guard
	if err := none.Allow(context.Background(), KindForge, "other.test", 443); err != nil {
		t.Fatalf("no guard: %v", err)
	}
}
