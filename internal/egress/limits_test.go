package egress

import (
	"bufio"
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

func startWith(t *testing.T, c Config, o Options) (*Proxy, *recorder) {
	t.Helper()
	rec := &recorder{}
	o.Policy, o.Record = mustCompile(t, c), rec.add
	if o.Resolve == nil {
		o.Resolve = fakeDNS
	}
	o.DialTimeout = 2 * time.Second
	p, err := Start(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, rec
}

// events returns what was recorded that f selects.
func (r *recorder) events(f func(Event) bool) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.evs {
		if f(e) {
			out = append(out, e)
		}
	}
	return out
}

func (r *recorder) waitFor(t *testing.T, what string, f func(Event) bool) Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if evs := r.events(f); len(evs) > 0 {
			return evs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no event: %s; have %+v", what, r.events(func(Event) bool { return true }))
	return Event{}
}

func proxyAuth(t *testing.T, p *Proxy, call string) string {
	t.Helper()
	r, _ := http.NewRequest("GET", "http://x", nil)
	r.SetBasicAuth(call, issue(t, p, call).token)
	return r.Header.Get("Authorization")
}

// send writes raw to the proxy and returns its status line.
func send(t *testing.T, p *Proxy, raw string) string {
	t.Helper()
	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, raw)
	line, _ := bufio.NewReader(c).ReadString('\n')
	return line
}

// What is refused before the policy reads it, a head that is not HTTP or
// is too large, is recorded too.
func TestProxyRecordsRefusalsBeforeThePolicy(t *testing.T) {
	p, rec := startProxy(t, Config{Default: "allow"})
	if s := send(t, p, "NOT AN HTTP REQUEST\r\n\r\n"); !strings.Contains(s, " 400 ") {
		t.Fatalf("garbage: %q", s)
	}
	rec.waitFor(t, "parse refusal", func(e Event) bool {
		return e.Kind == "request" && e.Rule == "parse" && e.Decision == Deny && strings.Contains(e.Reason, "not HTTP")
	})
	big := "GET http://allowed.test/ HTTP/1.1\r\nX-Pad: " + strings.Repeat("a", maxHeader+10) + "\r\n\r\n"
	if s := send(t, p, big); !strings.Contains(s, " 400 ") {
		t.Fatalf("oversize: %q", s)
	}
	rec.waitFor(t, "oversize refusal", func(e Event) bool {
		return e.Rule == "parse" && strings.Contains(e.Reason, "over")
	})
}

// The bound is 256 connections at once: all 256 are served, the next is
// closed unread and recorded, and a freed slot is taken again.
func TestProxyConnectionCap(t *testing.T) {
	p, rec := startProxy(t, Config{Default: "allow"})
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	for range 256 {
		c, err := net.Dial("tcp", p.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	deadline := time.Now().Add(10 * time.Second)
	for p.Served() < 256 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := p.Served(); n != 256 {
		t.Fatalf("served %d of 256 connections within the bound", n)
	}
	over, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = over.Close() }()
	_ = over.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := over.Read(make([]byte, 1)); n != 0 || err == nil || isTimeout(err) {
		t.Fatalf("connection 257 was not closed at once: %d %v", n, err)
	}
	rec.waitFor(t, "cap refusal", func(e Event) bool { return e.Rule == "cap" && e.Kind == "request" && e.Decision == Deny })
	if p.Served() != 256 {
		t.Fatalf("connection 257 was served")
	}
	_ = held[0].Close()
	deadline = time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", p.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		if waitServed(p, 257, 200*time.Millisecond) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a freed slot was not taken again")
		}
	}
}

func waitServed(p *Proxy, n int64, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if p.Served() >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Repeated denials and auth failures are recorded the first few times in
// an interval, then as one summary with the count.
func TestProxyRateLimitsRepeatedDenials(t *testing.T) {
	p, rec := startWith(t, Config{}, Options{Burst: 3, Interval: 300 * time.Millisecond})
	auth := proxyAuth(t, p, "call-flood")
	for i := range 10 {
		s := send(t, p, fmt.Sprintf("GET http://denied.test/x%d HTTP/1.1\r\nHost: denied.test\r\nProxy-Authorization: %s\r\n\r\n", i, auth))
		if !strings.Contains(s, " 403 ") {
			t.Fatalf("denied: %q", s)
		}
		if s := send(t, p, "GET http://denied.test/ HTTP/1.1\r\nHost: denied.test\r\n\r\n"); !strings.Contains(s, " 407 ") {
			t.Fatalf("auth: %q", s)
		}
	}
	sum := rec.waitFor(t, "denial summary", func(e Event) bool { return e.Host == "denied.test" && e.Repeats > 0 })
	authSum := rec.waitFor(t, "auth summary", func(e Event) bool { return e.Kind == "auth" && e.Repeats > 0 })
	one := rec.events(func(e Event) bool { return e.Host == "denied.test" && e.Repeats == 0 })
	auths := rec.events(func(e Event) bool { return e.Kind == "auth" && e.Repeats == 0 })
	if len(one) != 3 || sum.Repeats != 7 || len(auths) != 3 || authSum.Repeats != 7 {
		t.Fatalf("denials %d + %d, auth failures %d + %d; want 3 + 7 each", len(one), sum.Repeats, len(auths), authSum.Repeats)
	}
	if sum.CallID != "" || sum.Path != "" || !strings.Contains(sum.Reason, "7 more") {
		t.Fatalf("summary: %+v", sum)
	}
	if v, ok := sum.Payload()["repeats"]; !ok || v != int64(7) {
		t.Fatalf("payload: %v", sum.Payload())
	}
	// A new interval records the first few again.
	if s := send(t, p, "GET http://denied.test/again HTTP/1.1\r\nHost: denied.test\r\nProxy-Authorization: "+auth+"\r\n\r\n"); !strings.Contains(s, " 403 ") {
		t.Fatal(s)
	}
	rec.waitFor(t, "a denial in the next interval", func(e Event) bool { return e.Path == "/again" })
}

// Varying the target does not lift the limit: at most 100 denials are
// recorded one by one in an interval, the rest counted.
func TestProxyRateLimitHoldsAcrossKinds(t *testing.T) {
	p, rec := startWith(t, Config{}, Options{Interval: time.Hour})
	auth := proxyAuth(t, p, "call-vary")
	for i := range 130 {
		_ = send(t, p, fmt.Sprintf("CONNECT h%d.test:443 HTTP/1.1\r\nProxy-Authorization: %s\r\n\r\n", i, auth))
	}
	if n := len(rec.events(func(Event) bool { return true })); n != maxPerInterval {
		t.Fatalf("%d denials recorded one by one, want %d", n, maxPerInterval)
	}
	_ = p.Close()
	var counted int64
	for _, e := range rec.events(func(e Event) bool { return e.Repeats > 0 }) {
		counted += e.Repeats
	}
	if counted != 30 {
		t.Fatalf("the summaries at close count %d, want 30", counted)
	}
}

// With record_paths false, the record holds no path.
func TestProxyRecordPathsOff(t *testing.T) {
	no := false
	p, rec := startWith(t, Config{RecordPaths: &no}, Options{})
	auth := proxyAuth(t, p, "call-np")
	_ = send(t, p, "GET http://denied.test/reset?token=abc HTTP/1.1\r\nHost: denied.test\r\nProxy-Authorization: "+auth+"\r\n\r\n")
	e := rec.wait(t, "denied.test")
	if e.Path != "" || e.Method != "GET" || strings.Contains(fmt.Sprint(e.Payload()), "reset") {
		t.Fatalf("a path was recorded: %+v", e)
	}
}

// A tunnel, and a forwarded response, with no bytes either way for the
// idle time are closed, and the record says so.
func TestProxyIdleTimeout(t *testing.T) {
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
			// Holds the connection and says nothing.
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "part")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer stall.Close()
	sport := portOf(t, stall.URL)

	p, rec := startWith(t, Config{Rules: []Rule{
		{Host: "allowed.test", Ports: []int{port, sport}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}}, Options{Idle: 300 * time.Millisecond})
	auth := proxyAuth(t, p, "call-idle")

	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	start := time.Now()
	_, _ = fmt.Fprintf(c, "CONNECT allowed.test:%d HTTP/1.1\r\nProxy-Authorization: %s\r\n\r\n", port, auth)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, _ := io.ReadAll(c)
	if !strings.Contains(string(got), "200 Connection established") || time.Since(start) > 5*time.Second {
		t.Fatalf("tunnel not closed when idle: %q after %s", got, time.Since(start))
	}
	e := rec.waitFor(t, "idle tunnel", func(e Event) bool { return e.Kind == "connect" && e.Host == "allowed.test" })
	if !strings.Contains(e.Reason, "no bytes either way") {
		t.Fatalf("record: %+v", e)
	}

	h, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	start = time.Now()
	_, _ = fmt.Fprintf(h, "GET http://allowed.test:%d/ HTTP/1.1\r\nHost: allowed.test\r\nProxy-Authorization: %s\r\n\r\n", sport, auth)
	_ = h.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, _ = io.ReadAll(h)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("forwarded response not closed when idle: %q", got)
	}
	e = rec.waitFor(t, "idle request", func(e Event) bool { return e.Kind == "http" && e.Host == "allowed.test" })
	if !strings.Contains(e.Reason, "no bytes either way") {
		t.Fatalf("record: %+v", e)
	}
}

// The name is resolved once, and the address checked is the one dialled:
// a second answer, here an internal address, is never asked for.
func TestProxyResolvesOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	port := portOf(t, srv.URL)
	var calls atomic.Int32
	resolve := func(_ context.Context, host string) ([]netip.Addr, error) {
		if host != "rebind.test" {
			return nil, fmt.Errorf("no such host %s", host)
		}
		if calls.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
	}
	p, rec := startWith(t, Config{Rules: []Rule{
		{Host: "rebind.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}}, Options{Resolve: resolve})
	c := client(t, callURL(t, p, "call-rb"), nil)
	resp, err := c.Get(fmt.Sprintf("http://rebind.test:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || calls.Load() != 1 {
		t.Fatalf("status %d after %d lookups; want 200 after 1", resp.StatusCode, calls.Load())
	}
	// A tunnel as well.
	calls.Store(0)
	auth := proxyAuth(t, p, "call-rb")
	if s := send(t, p, fmt.Sprintf("CONNECT rebind.test:%d HTTP/1.1\r\nProxy-Authorization: %s\r\n\r\n", port, auth)); !strings.Contains(s, " 200 ") || calls.Load() != 1 {
		t.Fatalf("tunnel: %q after %d lookups", s, calls.Load())
	}
	if e := rec.wait(t, "rebind.test"); e.IP != "127.0.0.1" {
		t.Fatalf("dialled %s", e.IP)
	}
}

// A rule narrowed by method or path cannot allow a CONNECT, whose method
// and path the proxy never sees.
func TestProxyNarrowedAllowRefusesConnect(t *testing.T) {
	p, rec := startProxy(t, Config{Rules: []Rule{
		{Host: "allowed.test", Methods: []string{"GET"}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
		{Host: "allowed.test", Paths: []string{"/v1/*"}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}})
	auth := proxyAuth(t, p, "call-n")
	if s := send(t, p, "CONNECT allowed.test:443 HTTP/1.1\r\nProxy-Authorization: "+auth+"\r\n\r\n"); !strings.Contains(s, " 403 ") {
		t.Fatalf("CONNECT under a narrowed allow: %q", s)
	}
	if e := rec.wait(t, "allowed.test"); e.Decision != Deny || e.Rule != "default" {
		t.Fatalf("record: %+v", e)
	}
}

// Asked to, the proxy holds the same port on [::1], so nothing else can
// take the half of "localhost" a sandbox allows.
func TestProxyHoldsIPv6Loopback(t *testing.T) {
	if !ipv6Loopback() {
		t.Skip("no IPv6 loopback here")
	}
	p, _ := startWith(t, Config{}, Options{IPv6Loopback: true})
	v6 := net.JoinHostPort("::1", fmt.Sprint(p.Addr().Port()))
	if ln, err := net.Listen("tcp", v6); err == nil {
		_ = ln.Close()
		t.Fatalf("%s was free for another process", v6)
	}
	c, err := net.Dial("tcp", v6)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "CONNECT a.test:443 HTTP/1.1\r\n\r\n")
	if s, _ := bufio.NewReader(c).ReadString('\n'); !strings.Contains(s, " 407 ") {
		t.Fatalf("[::1] is not the proxy: %q", s)
	}
}

// Allowed decisions past the budget in an interval are counted, not
// recorded one by one, and recorded as a summary with their bytes.
func TestProxyBudgetsAllowedDecisions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	port := portOf(t, srv.URL)
	p, rec := startWith(t, Config{Rules: []Rule{
		{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}}, Options{AllowBudget: 5, Interval: time.Hour})
	auth := proxyAuth(t, p, "call-loop")
	for i := range 12 {
		s := send(t, p, fmt.Sprintf("GET http://allowed.test:%d/x%d HTTP/1.1\r\nHost: allowed.test\r\nProxy-Authorization: %s\r\n\r\n", port, i, auth))
		if !strings.Contains(s, " 200 ") {
			t.Fatalf("allowed: %q", s)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(rec.events(func(Event) bool { return true })) < 5 {
		time.Sleep(10 * time.Millisecond)
	}
	_ = p.Close()
	one := rec.events(func(e Event) bool { return e.Decision == Allow && e.Repeats == 0 })
	sums := rec.events(func(e Event) bool { return e.Decision == Allow && e.Repeats > 0 })
	if len(one) != 5 || len(sums) != 1 || sums[0].Repeats != 7 {
		t.Fatalf("allowed one by one %d, summaries %+v; want 5 and one of 7", len(one), sums)
	}
	if s := sums[0]; s.Host != "allowed.test" || s.CallID != "" || s.Path != "" || !strings.Contains(s.Reason, "7 more allowed") {
		t.Fatalf("summary: %+v", s)
	}
}

// Past maxKinds, a new kind shares its side's overflow, recorded as one
// summary with its bytes, so the kinds held stay bounded; no count keeps a path.
func TestLimiterKindsOverflow(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	l := newLimiter(1, 1, time.Hour, func(e Event) { mu.Lock(); got = append(got, e); mu.Unlock() })
	long := "/" + strings.Repeat("p", 4096)
	for i := range maxKinds + 5 {
		l.record(Event{Kind: "connect", Decision: Deny, Rule: "default", Host: fmt.Sprintf("h%d.test", i), Port: 443, Path: long})
	}
	l.record(Event{Kind: "connect", Decision: Allow, Rule: "r", Host: "a0.test", Port: 443})
	for i := range maxKinds + 3 {
		l.record(Event{Kind: "connect", Decision: Allow, Rule: "r", Host: fmt.Sprintf("a%d.test", i+1), Port: 443, BytesIn: 10, BytesOut: 1, Path: long})
	}
	l.mu.Lock()
	n := len(l.kinds)
	for k, tl := range l.kinds {
		if tl.sample.Path != "" || tl.sample.CallID != "" {
			l.mu.Unlock()
			t.Fatalf("kind %s keeps %q", k, tl.sample.Path[:10])
		}
	}
	l.mu.Unlock()
	if n != 2*maxKinds+2 {
		t.Fatalf("%d kinds held, want %d per side and the two overflows", n, maxKinds)
	}
	l.stop()
	mu.Lock()
	defer mu.Unlock()
	var deny, allow *Event
	for i, e := range got {
		if e.Kind == "summary" && e.Decision == Deny {
			deny = &got[i]
		}
		if e.Kind == "summary" && e.Decision == Allow {
			allow = &got[i]
		}
	}
	if deny == nil || deny.Repeats != 5 || deny.Rule != "rate" {
		t.Fatalf("denial overflow: %+v", deny)
	}
	if allow == nil || allow.Repeats != 3 || allow.BytesIn != 30 || allow.BytesOut != 3 {
		t.Fatalf("allowed overflow: %+v", allow)
	}
}

// Allowed traffic past its budget, over many hosts, leaves the denials their
// kinds: a new denied host is still recorded with its host.
func TestLimiterAllowedFloodKeepsDenialKinds(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	l := newLimiter(1, 1, time.Hour, func(e Event) { mu.Lock(); got = append(got, e); mu.Unlock() })
	defer l.stop()
	for i := range maxKinds + 50 {
		l.record(Event{Kind: "connect", Decision: Allow, Rule: "*.example.com", Host: fmt.Sprintf("s%d.example.com", i), Port: 443})
	}
	l.record(Event{Kind: "connect", Decision: Deny, Rule: "default", Host: "exfil.test", Port: 443})
	mu.Lock()
	defer mu.Unlock()
	last := got[len(got)-1]
	if last.Decision != Deny || last.Host != "exfil.test" || last.Repeats != 0 {
		t.Fatalf("the denial was not recorded with its host: %+v", last)
	}
}
