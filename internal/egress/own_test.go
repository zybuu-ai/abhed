package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// payloads collects what a Caller's Record is given.
type payloads struct {
	mu  sync.Mutex
	got []map[string]any
}

func (p *payloads) rec(event string, m map[string]any) error {
	if event != EventName {
		return fmt.Errorf("event %s", event)
	}
	p.mu.Lock()
	p.got = append(p.got, m)
	p.mu.Unlock()
	return nil
}

func (p *payloads) all() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.got...)
}

// ownServer is a loopback server counting its requests, and its port.
func ownServer(t *testing.T) (*httptest.Server, int, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "own ok "+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv, int(netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://")).Port()), &hits
}

func ownGuard(t *testing.T, c Config, o GuardOptions) *Guard {
	t.Helper()
	if o.Policy == nil && o.Err == nil {
		o.Policy = mustCompile(t, c)
	}
	if o.Resolve == nil {
		o.Resolve = fakeDNS
	}
	g := NewGuard(o)
	t.Cleanup(g.Close)
	return g
}

func ownGet(t *testing.T, tr *Transport, ctx context.Context, url string) (string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

// An allowed request goes through and is recorded in its session's record
// with the call, the address dialled, the method and the path.
func TestOwnAllowedIsRecorded(t *testing.T) {
	_, port, _ := ownServer(t)
	g := ownGuard(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}}, GuardOptions{})
	tr := &Transport{Kind: KindWebSearch, Guard: g}
	var p payloads
	ctx := WithCaller(context.Background(), Caller{Session: "s1", CallID: "call-1", Record: p.rec})
	body, err := ownGet(t, tr, ctx, fmt.Sprintf("http://allowed.test:%d/q", port))
	if err != nil || body != "own ok /q" {
		t.Fatalf("allowed request: %q, %v", body, err)
	}
	got := p.all()
	if len(got) != 1 {
		t.Fatalf("records: %v", got)
	}
	e := got[0]
	for k, v := range map[string]any{"session": "s1", "call_id": "call-1", "kind": KindWebSearch, "host": "allowed.test",
		"decision": "allow", "ip": "127.0.0.1", "method": "GET", "path": "/q"} {
		if e[k] != v {
			t.Errorf("%s = %v, want %v (%v)", k, e[k], v, e)
		}
	}
}

// A request no rule allows is refused before any connection, and recorded.
func TestOwnDeniedIsRecordedAndNeverSent(t *testing.T) {
	_, port, hits := ownServer(t)
	g := ownGuard(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}}, GuardOptions{})
	tr := &Transport{Kind: KindMCP, Guard: g}
	var p payloads
	ctx := WithCaller(context.Background(), Caller{Session: "s1", CallID: "call-2", Record: p.rec})
	_, err := ownGet(t, tr, ctx, fmt.Sprintf("http://denied.test:%d/", port))
	var de *DeniedError
	if !errors.As(err, &de) || de.Rule != "default" {
		t.Fatalf("denied request: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("a denied request reached the server")
	}
	got := p.all()
	if len(got) != 1 || got[0]["decision"] != "deny" || got[0]["call_id"] != "call-2" || got[0]["kind"] != KindMCP {
		t.Fatalf("records: %v", got)
	}
}

// An allowed host that resolves to an internal address no rule names is
// refused at the dial, as the proxy refuses it.
func TestOwnRefusesInternalAddresses(t *testing.T) {
	_, port, hits := ownServer(t)
	g := ownGuard(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "allow"},
		{Host: "mixed.test", Ports: []int{port}, Decision: "allow"}}}, GuardOptions{})
	tr := &Transport{Kind: KindWebFetch, Guard: g}
	var p payloads
	ctx := WithCaller(context.Background(), Caller{Session: "s1", Record: p.rec})
	for _, host := range []string{"allowed.test", "mixed.test", "127.0.0.1"} {
		_, err := ownGet(t, tr, ctx, fmt.Sprintf("http://%s:%d/", host, port))
		var de *DeniedError
		if !errors.As(err, &de) {
			t.Fatalf("%s: %v", host, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("an internal address was reached")
	}
	for _, e := range p.all() {
		if e["decision"] != "deny" {
			t.Errorf("record: %v", e)
		}
	}
}

// A pooled connection dialled under one rule's allow_ips is not reused by
// a request whose rule names none.
func TestOwnReusedConnectionIsChecked(t *testing.T) {
	var hits, conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "own ok "+r.URL.Path)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	port := int(netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://")).Port())
	g := ownGuard(t, Config{Rules: []Rule{
		{Host: "internal.test", Ports: []int{port}, Paths: []string{"/a"}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
		{Host: "internal.test", Ports: []int{port}, Paths: []string{"/b"}, Decision: "allow"},
	}}, GuardOptions{})
	tr := &Transport{Kind: KindMCP, Guard: g}
	ctx := context.Background()
	if body, err := ownGet(t, tr, ctx, fmt.Sprintf("http://internal.test:%d/a", port)); err != nil || body != "own ok /a" {
		t.Fatalf("/a: %q %v", body, err)
	}
	_, err := ownGet(t, tr, ctx, fmt.Sprintf("http://internal.test:%d/b", port))
	var de *DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("/b reached the internal address on a pooled connection: %v", err)
	}
	// One connection: /b was given /a's pooled connection and refused on it.
	if hits.Load() != 1 || conns.Load() != 1 {
		t.Fatalf("hits %d, connections %d", hits.Load(), conns.Load())
	}
}

// The model client's own endpoint is allowed implicitly, loopback included,
// and recorded by the rule "model"; another host from it is judged.
func TestOwnModelEndpointIsAllowedImplicitly(t *testing.T) {
	srv, port, _ := ownServer(t)
	g := ownGuard(t, Config{}, GuardOptions{})
	tr := &Transport{Kind: KindModel, Guard: g, Model: func() []string { return []string{srv.URL + "/v1"} }}
	var p payloads
	ctx := WithCaller(context.Background(), Caller{Session: "s1", Record: p.rec})
	if body, err := ownGet(t, tr, ctx, srv.URL+"/v1/chat"); err != nil || body != "own ok /v1/chat" {
		t.Fatalf("model endpoint: %q %v", body, err)
	}
	if _, err := ownGet(t, tr, ctx, fmt.Sprintf("http://allowed.test:%d/", port)); err == nil {
		t.Fatal("the model client reached another host no rule allows")
	}
	got := p.all()
	if len(got) != 2 || got[0]["rule"] != RuleModel || got[0]["decision"] != "allow" || got[0]["kind"] != KindModel ||
		got[1]["decision"] != "deny" {
		t.Fatalf("records: %v", got)
	}
}

// A policy that could not be loaded refuses every request but the model's.
func TestOwnFailsClosedWithoutAPolicy(t *testing.T) {
	srv, port, hits := ownServer(t)
	g := ownGuard(t, Config{}, GuardOptions{Err: errors.New("broken rules")})
	var p payloads
	ctx := WithCaller(context.Background(), Caller{Session: "s1", Record: p.rec})
	model := &Transport{Kind: KindModel, Guard: g, Model: func() []string { return []string{srv.URL} }}
	if _, err := ownGet(t, model, ctx, srv.URL+"/"); err != nil {
		t.Fatalf("the model was refused: %v", err)
	}
	fetch := &Transport{Kind: KindWebFetch, Guard: g}
	_, err := ownGet(t, fetch, ctx, fmt.Sprintf("http://allowed.test:%d/", port))
	var de *DeniedError
	if !errors.As(err, &de) || de.Rule != "policy" || !strings.Contains(de.Reason, "broken rules") {
		t.Fatalf("without a policy: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits %d", hits.Load())
	}
}

// Audit mode lets a denied request through and records it as would_deny.
func TestOwnAuditMode(t *testing.T) {
	_, port, hits := ownServer(t)
	g := ownGuard(t, Config{Mode: "audit", Rules: []Rule{
		{Host: "audit.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
		{Host: "audit.test", Ports: []int{port}, Paths: []string{"/x"}, Decision: "deny"},
	}}, GuardOptions{})
	var p payloads
	ctx := WithCaller(context.Background(), Caller{Session: "s1", Record: p.rec})
	if _, err := ownGet(t, &Transport{Kind: KindWebSearch, Guard: g}, ctx, fmt.Sprintf("http://audit.test:%d/x", port)); err != nil {
		t.Fatalf("audit mode refused: %v", err)
	}
	if got := p.all(); len(got) != 1 || got[0]["decision"] != "would_deny" || hits.Load() != 1 {
		t.Fatalf("records: %v, hits %d", got, hits.Load())
	}
}

// Without a guard in force the transport is the client's own, unchanged.
func TestOwnUnchangedOutsideTheAllowlist(t *testing.T) {
	if g, _ := Installed(); g != nil {
		t.Skip("a guard is installed")
	}
	srv, _, hits := ownServer(t)
	if body, err := ownGet(t, &Transport{Kind: KindWebSearch}, context.Background(), srv.URL+"/x"); err != nil || body != "own ok /x" {
		t.Fatalf("%q %v", body, err)
	}
	if hits.Load() != 1 {
		t.Fatal("not sent")
	}
}

// Install puts a guard in force for transports that name none, until undone.
func TestOwnInstall(t *testing.T) {
	_, port, hits := ownServer(t)
	g := ownGuard(t, Config{}, GuardOptions{})
	undo := Install(g)
	defer undo()
	if in, err := Installed(); in != g || err != nil {
		t.Fatal("not installed")
	}
	if _, err := ownGet(t, &Transport{Kind: KindWebSearch}, context.Background(), fmt.Sprintf("http://127.0.0.1:%d/", port)); err == nil {
		t.Fatal("the installed guard did not judge the request")
	}
	undo()
	if in, _ := Installed(); in != nil || hits.Load() != 0 {
		t.Fatalf("installed %v, hits %d", in, hits.Load())
	}
}

// Allowed decisions over the budget are counted, and the summary is
// recorded when the guard closes.
func TestOwnRecordIsRateLimited(t *testing.T) {
	_, port, _ := ownServer(t)
	g := NewGuard(GuardOptions{Policy: mustCompile(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "allow",
		AllowIPs: []string{"127.0.0.1"}}}}), Resolve: fakeDNS, AllowBudget: 2, Interval: time.Hour})
	var p payloads
	ctx := WithCaller(context.Background(), Caller{Session: "s1", Record: p.rec})
	tr := &Transport{Kind: KindWebSearch, Guard: g}
	for i := range 5 {
		if _, err := ownGet(t, tr, ctx, fmt.Sprintf("http://allowed.test:%d/%d", port, i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(p.all()); n != 2 {
		t.Fatalf("%d recorded one by one, want 2", n)
	}
	g.Close()
	got := p.all()
	if len(got) != 3 || fmt.Sprint(got[2]["repeats"]) != "3" || got[2]["session"] != "s1" {
		t.Fatalf("records: %v", got)
	}
}

// A request naming a guard, or Unguarded, is judged by it whatever is
// installed; one naming none is refused while two guards are installed.
func TestOwnGuardFromTheRequest(t *testing.T) {
	_, port, hits := ownServer(t)
	allowing := ownGuard(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}}, GuardOptions{})
	denying := ownGuard(t, Config{}, GuardOptions{})
	defer Install(denying)()
	tr := &Transport{Kind: KindWebSearch}
	url := fmt.Sprintf("http://allowed.test:%d/", port)
	if _, err := ownGet(t, tr, WithGuard(context.Background(), allowing), url); err != nil {
		t.Fatalf("the request's guard was not used: %v", err)
	}
	if _, err := ownGet(t, tr, WithGuard(context.Background(), Unguarded), fmt.Sprintf("http://127.0.0.1:%d/", port)); err != nil {
		t.Fatalf("an unguarded request was judged: %v", err)
	}
	if _, err := ownGet(t, tr, context.Background(), url); err == nil {
		t.Fatal("the installed guard did not judge a request naming none")
	}
	defer Install(allowing)()
	if _, err := ownGet(t, tr, context.Background(), url); !errors.Is(err, errAmbiguous) {
		t.Fatalf("a request naming no guard with two installed: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits %d", hits.Load())
	}
}

// closedBody notes that a request body was closed.
type closedBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *closedBody) Close() error { b.closed.Store(true); return nil }

// A refused request's body is closed, as a RoundTripper must.
func TestOwnDeniedRequestBodyIsClosed(t *testing.T) {
	g := ownGuard(t, Config{}, GuardOptions{})
	for _, url := range []string{"http://denied.test/", "http://[::1%25x]/"} {
		body := &closedBody{Reader: strings.NewReader("x")}
		req, _ := http.NewRequest(http.MethodPost, url, body)
		if _, err := (&Transport{Kind: KindMCP, Guard: g}).RoundTrip(req); err == nil {
			t.Fatalf("%s was sent", url)
		}
		if !body.closed.Load() {
			t.Errorf("%s: the body was left open", url)
		}
	}
}

// Close drops the guard's pools: it holds none of a client's connections.
func TestOwnCloseDropsPools(t *testing.T) {
	_, port, _ := ownServer(t)
	g := ownGuard(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}}, GuardOptions{})
	tr := &Transport{Kind: KindWebSearch, Guard: g}
	if _, err := ownGet(t, tr, context.Background(), fmt.Sprintf("http://allowed.test:%d/", port)); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	n := len(g.pools)
	g.mu.Unlock()
	g.Close()
	g.mu.Lock()
	after := len(g.pools)
	g.mu.Unlock()
	if n != 1 || after != 0 {
		t.Fatalf("pools %d, after close %d", n, after)
	}
	if _, err := ownGet(t, tr, context.Background(), fmt.Sprintf("http://allowed.test:%d/", port)); err != nil {
		t.Fatalf("after close: %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pools) != 0 {
		t.Fatal("a closed guard kept a pool")
	}
}
