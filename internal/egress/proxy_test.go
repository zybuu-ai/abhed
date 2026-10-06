package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder collects the proxy's events.
type recorder struct {
	mu  sync.Mutex
	evs []Event
}

func (r *recorder) add(e Event) {
	r.mu.Lock()
	r.evs = append(r.evs, e)
	r.mu.Unlock()
}

// wait returns the first event for host, waiting for it a short while.
func (r *recorder) wait(t *testing.T, host string) Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, e := range r.evs {
			if e.Host == host {
				r.mu.Unlock()
				return e
			}
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no event for %s", host)
	return Event{}
}

// fakeDNS resolves the test names: each to loopback, where the test
// servers are, and mixed.test to a public and a private address.
func fakeDNS(_ context.Context, host string) ([]netip.Addr, error) {
	switch host {
	case "mixed.test":
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.1")}, nil
	case "allowed.test", "denied.test", "internal.test", "audit.test":
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	return nil, fmt.Errorf("no such host %s", host)
}

func startProxy(t *testing.T, c Config) (*Proxy, *recorder) {
	t.Helper()
	pol := mustCompile(t, c)
	rec := &recorder{}
	p, err := Start(Options{Policy: pol, Record: rec.add, Resolve: fakeDNS, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, rec
}

func portOf(t *testing.T, raw string) int {
	t.Helper()
	u, _ := url.Parse(raw)
	ap, err := netip.ParseAddrPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	return int(ap.Port())
}

func client(t *testing.T, proxyURL string, tlsConf *tls.Config) *http.Client {
	t.Helper()
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: tlsConf}}
}

func TestProxyPlainHTTP(t *testing.T) {
	var sawAuth, sawHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth, sawHost = r.Header.Get("Proxy-Authorization"), r.Host
		_, _ = io.WriteString(w, "hello from "+r.URL.Path)
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)
	p, rec := startProxy(t, Config{Rules: []Rule{
		{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
		{Host: "internal.test", Ports: []int{port}, Decision: "allow"},
	}})
	c := client(t, p.URL("call-1"), nil)

	resp, err := c.Get(fmt.Sprintf("http://allowed.test:%d/greet?token=secret", port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello from /greet" {
		t.Fatalf("allowed: %d %q", resp.StatusCode, body)
	}
	if sawAuth != "" {
		t.Fatal("the proxy credentials reached the origin")
	}
	if sawHost != fmt.Sprintf("allowed.test:%d", port) {
		t.Fatalf("origin saw Host %q", sawHost)
	}
	ev := rec.wait(t, "allowed.test")
	if ev.Decision != Allow || ev.CallID != "call-1" || ev.Kind != "http" || ev.Method != "GET" || ev.Path != "/greet" ||
		ev.IP != "127.0.0.1" || ev.BytesIn == 0 || ev.BytesOut == 0 || !strings.HasPrefix(ev.Rule, "rules[0]") {
		t.Fatalf("event: %+v", ev)
	}
	if pay := fmt.Sprint(ev.Payload()); strings.Contains(pay, "secret") {
		t.Fatalf("the query reached the record: %s", pay)
	}

	// Not in the rules: refused with a 403, and recorded.
	resp, err = c.Get(fmt.Sprintf("http://denied.test:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied: %d", resp.StatusCode)
	}
	if ev := rec.wait(t, "denied.test"); ev.Decision != Deny || ev.Rule != "default" || ev.BytesOut != 0 {
		t.Fatalf("denied event: %+v", ev)
	}

	// Allowed by name, but it resolves to loopback, which no rule names.
	resp, err = c.Get(fmt.Sprintf("http://internal.test:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("internal: %d", resp.StatusCode)
	}
	if ev := rec.wait(t, "internal.test"); ev.Decision != Deny || !strings.Contains(ev.Reason, "loopback") {
		t.Fatalf("internal event: %+v", ev)
	}
}

func TestProxyConnect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "tls ok")
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)
	p, rec := startProxy(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "deny"},
		{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.0/8"}}}})
	// The deny wins over the allow; a second proxy allows.
	tlsConf := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConf.ServerName = "example.com"
	if resp, err := client(t, p.URL("c"), tlsConf).Get(fmt.Sprintf("https://allowed.test:%d/", port)); err == nil {
		_ = resp.Body.Close()
		t.Fatal("the deny rule did not refuse the tunnel")
	}
	if ev := rec.wait(t, "allowed.test"); ev.Decision != Deny || ev.Kind != "connect" || ev.Method != "" {
		t.Fatalf("connect deny event: %+v", ev)
	}

	p2, rec2 := startProxy(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.0/8"}}}})
	resp, err := client(t, p2.URL("c2"), tlsConf).Get(fmt.Sprintf("https://allowed.test:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "tls ok" {
		t.Fatalf("body %q", body)
	}
	p2.Close() // ends the kept-alive tunnel, which writes its record
	ev := rec2.wait(t, "allowed.test")
	if ev.Decision != Allow || ev.Kind != "connect" || ev.CallID != "c2" || ev.BytesIn == 0 || ev.BytesOut == 0 {
		t.Fatalf("connect allow event: %+v", ev)
	}
}

func TestProxyNeedsTheToken(t *testing.T) {
	p, rec := startProxy(t, Config{Default: "allow"})
	for _, u := range []string{"http://" + p.Addr().String(), "http://call:wrong@" + p.Addr().String()} {
		resp, err := client(t, u, nil).Get("http://allowed.test:1/")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusProxyAuthRequired {
			t.Fatalf("%s: %d", u, resp.StatusCode)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.evs) != 2 || rec.evs[0].Kind != "auth" || rec.evs[0].Decision != Deny {
		t.Fatalf("events: %+v", rec.evs)
	}
}

func TestProxyAuditModeLetsThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	port := portOf(t, srv.URL)
	p, rec := startProxy(t, Config{Mode: "audit", Rules: []Rule{{Host: "audit.test", Ports: []int{port}, Decision: "deny", AllowIPs: []string{"127.0.0.1"}}}})
	resp, err := client(t, p.URL("a"), nil).Get(fmt.Sprintf("http://audit.test:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	// The rule denies and names no internal address, so the loopback
	// address is still refused: audit mode never lifts the address check.
	if ev := rec.wait(t, "audit.test"); resp.StatusCode != http.StatusForbidden || ev.Decision != Deny {
		t.Fatalf("audit over a refused address: %d %+v", resp.StatusCode, ev)
	}
	p2, rec2 := startProxy(t, Config{Mode: "audit", Rules: []Rule{
		{Host: "audit.test", Ports: []int{port}, Methods: []string{"POST"}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
		{Host: "audit.test", Ports: []int{port}, Methods: []string{"GET"}, Decision: "deny"}}})
	resp, err = client(t, p2.URL("a"), nil).Get(fmt.Sprintf("http://audit.test:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if ev := rec2.wait(t, "audit.test"); resp.StatusCode != http.StatusForbidden || ev.Decision != Deny {
		t.Fatalf("audit without a rule naming loopback: %d %+v", resp.StatusCode, ev)
	}
}

func TestProxyAuditModeRecordsWouldDeny(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	port := portOf(t, srv.URL)
	// An allow rule naming loopback, and a deny on one path: audit mode lets
	// the denied path through and records it as would_deny.
	p, rec := startProxy(t, Config{Mode: "audit", Rules: []Rule{
		{Host: "audit.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
		{Host: "audit.test", Ports: []int{port}, Paths: []string{"/blocked"}, Decision: "deny"}}})
	resp, err := client(t, p.URL("a"), nil).Get(fmt.Sprintf("http://audit.test:%d/blocked", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	ev := rec.wait(t, "audit.test")
	if resp.StatusCode != 200 || ev.Decision != WouldDeny || !strings.HasPrefix(ev.Rule, "rules[1]") {
		t.Fatalf("audit: %d %+v", resp.StatusCode, ev)
	}
}

func TestProxyRefusesMixedAddressesAndItself(t *testing.T) {
	p, rec := startProxy(t, Config{Rules: []Rule{
		{Host: "mixed.test", Decision: "allow"},
		{Host: "127.0.0.1", Ports: []int{1, 65535}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}})
	resp, err := client(t, p.URL("m"), nil).Get("http://mixed.test/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if ev := rec.wait(t, "mixed.test"); resp.StatusCode != http.StatusForbidden || !strings.Contains(ev.Reason, "10.0.0.1") {
		t.Fatalf("mixed: %d %+v", resp.StatusCode, ev)
	}
	// The proxy's own port, even where a rule names loopback.
	p2, rec2 := startProxy(t, Config{Default: "allow", Rules: []Rule{{Host: "127.0.0.1", Decision: "allow", AllowIPs: []string{"127.0.0.0/8"}}}})
	conn, err := net.Dial("tcp", p2.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	user, pass := creds(t, p2)
	r, _ := http.NewRequest("CONNECT", "http://"+p2.Addr().String(), nil)
	r.Host = p2.Addr().String()
	r.SetBasicAuth(user, pass)
	r.Header.Set("Proxy-Authorization", r.Header.Get("Authorization"))
	r.Header.Del("Authorization")
	if err := r.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err = http.ReadResponse(bufio.NewReader(conn), r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ev := rec2.wait(t, "127.0.0.1"); resp.StatusCode != http.StatusForbidden || !strings.Contains(ev.Reason, "this proxy itself") {
		t.Fatalf("self: %d %+v", resp.StatusCode, ev)
	}
}

func TestProxyMalformedRequests(t *testing.T) {
	p, rec := startProxy(t, Config{Default: "allow"})
	user, pass := creds(t, p)
	r, _ := http.NewRequest("GET", "http://x", nil)
	r.SetBasicAuth(user, pass)
	cred := r.Header.Get("Authorization")
	for _, line := range []string{
		"GET /relative HTTP/1.1",
		"GET http://example.com/../admin HTTP/1.1",
		"GET https://example.com/ HTTP/1.1",
		"CONNECT 2130706433:443 HTTP/1.1",
	} {
		conn, err := net.Dial("tcp", p.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(conn, "%s\r\nHost: example.com\r\nProxy-Authorization: %s\r\n\r\n", line, cred)
		status, _ := bufio.NewReader(conn).ReadString('\n')
		_ = conn.Close()
		if !strings.Contains(status, " 400 ") {
			t.Errorf("%q: %q", line, status)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.evs {
		if e.Decision != Deny || e.Rule != "parse" {
			t.Errorf("event %+v", e)
		}
	}
}

func TestProxyEnv(t *testing.T) {
	p, _ := startProxy(t, Config{})
	env := strings.Join(p.Env("toolu_1"), "\n")
	for _, k := range []string{"HTTP_PROXY=", "http_proxy=", "HTTPS_PROXY=", "https_proxy=", "NO_PROXY=", "no_proxy="} {
		if !strings.Contains(env, k) {
			t.Errorf("no %s", k)
		}
	}
	if !strings.Contains(env, "toolu_1:"+p.Token()+"@"+p.Addr().String()) {
		t.Errorf("proxy URL: %s", env)
	}
}

// creds are the user name and password in the proxy's URL.
func creds(t *testing.T, p *Proxy) (string, string) {
	t.Helper()
	u, err := url.Parse(p.URL("x"))
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := u.User.Password()
	return u.User.Username(), pass
}
