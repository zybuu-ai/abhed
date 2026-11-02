package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
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

// corpProxy stands in for a corporate proxy: it answers CONNECT and
// absolute-form requests, asks for want as Proxy-Authorization when set,
// records what it was asked for, and reaches every name on loopback.
type corpProxy struct {
	t    *testing.T
	ln   net.Listener
	want string

	mu   sync.Mutex
	seen []corpSeen
}

type corpSeen struct{ method, target, auth string }

func startCorp(t *testing.T, want string, tlsConf *tls.Config) *corpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if tlsConf != nil {
		ln = tls.NewListener(ln, tlsConf)
	}
	c := &corpProxy{t: t, ln: ln, want: want}
	go c.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return c
}

func (c *corpProxy) url(scheme string) *url.URL {
	return &url.URL{Scheme: scheme, Host: c.ln.Addr().String()}
}

func (c *corpProxy) requests() []corpSeen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]corpSeen(nil), c.seen...)
}

// saw reports whether the proxy was asked for anything naming host.
func (c *corpProxy) saw(host string) bool {
	for _, s := range c.requests() {
		if strings.Contains(s.target, host) {
			return true
		}
	}
	return false
}

func (c *corpProxy) serve() {
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return
		}
		go c.handle(conn)
	}
}

// loopback is host:port with the host replaced by 127.0.0.1.
func loopback(authority string) string {
	_, port, _ := net.SplitHostPort(authority)
	return net.JoinHostPort("127.0.0.1", port)
}

func (c *corpProxy) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		auth := req.Header.Get("Proxy-Authorization")
		target := req.RequestURI
		c.mu.Lock()
		c.seen = append(c.seen, corpSeen{method: req.Method, target: target, auth: auth})
		c.mu.Unlock()
		if c.want != "" && auth != c.want {
			_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"corp\"\r\nContent-Length: 0\r\n\r\n")
			continue
		}
		if req.Method == http.MethodConnect {
			up, err := net.Dial("tcp", loopback(req.Host))
			if err != nil {
				_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
				return
			}
			_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
			splice(conn, br, up)
			_ = up.Close()
			return
		}
		u, err := url.Parse(target)
		if err != nil || !u.IsAbs() {
			_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
			return
		}
		up, err := net.Dial("tcp", loopback(u.Host))
		if err != nil {
			return
		}
		req.Header.Del("Proxy-Authorization")
		req.RequestURI = ""
		req.Close = true
		_ = req.Write(up)
		resp, err := http.ReadResponse(bufio.NewReader(up), req)
		if err == nil {
			_ = resp.Write(conn)
			_ = resp.Body.Close()
		}
		_ = up.Close()
		return
	}
}

const corpUser, corpPass = "svc-abhed", "c0rp-pr0xy-pa55"

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// useUpstream puts an upstream to c in force for the test.
func useUpstream(t *testing.T, c *corpProxy, mutate func(*Upstream)) *Upstream {
	t.Helper()
	u := &Upstream{URL: c.url("http"), Credentials: func() (string, string, bool, error) {
		return corpUser, corpPass, true, nil
	}}
	if mutate != nil {
		mutate(u)
	}
	SetUpstream(u)
	t.Cleanup(func() { SetUpstream(nil) })
	return u
}

// noSecret fails the test where text holds the proxy's password or header.
func noSecret(t *testing.T, where, text string) {
	t.Helper()
	if strings.Contains(text, corpPass) || strings.Contains(text, basic(corpUser, corpPass)) ||
		strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(corpUser+":"+corpPass))) {
		t.Fatalf("the proxy credential reached %s: %s", where, text)
	}
}

// Abhed's own requests are judged first; the allowed ones go through the
// corporate proxy, CONNECT for https and absolute form for http, each with
// the credentials; the refused ones never reach it.
func TestUpstreamOwnRequestsChainAfterThePolicy(t *testing.T) {
	var originAuth string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originAuth = r.Header.Get("Proxy-Authorization")
		_, _ = io.WriteString(w, "plain ok")
	}))
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "tls ok")
	}))
	t.Cleanup(secure.Close)
	pport, sport := portOf(t, plain.URL), portOf(t, secure.URL)
	corp := startCorp(t, basic(corpUser, corpPass), nil)
	up := useUpstream(t, corp, nil)

	g := ownGuard(t, Config{Rules: []Rule{
		{Host: "allowed.test", Ports: []int{pport, sport}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}}, GuardOptions{})
	var rec payloads
	ctx := WithCaller(context.Background(), Caller{Session: "s", CallID: "c1", Record: rec.rec})

	pool := x509.NewCertPool()
	pool.AddCert(secure.Certificate())
	tr := &Transport{Kind: KindWebFetch, Guard: g, Base: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"}}}
	body, err := ownGet(t, tr, ctx, fmt.Sprintf("https://allowed.test:%d/x", sport))
	if err != nil || body != "tls ok" {
		t.Fatalf("https through the proxy: %q %v", body, err)
	}
	body, err = ownGet(t, tr, ctx, fmt.Sprintf("http://allowed.test:%d/y", pport))
	if err != nil || body != "plain ok" {
		t.Fatalf("http through the proxy: %q %v", body, err)
	}
	if originAuth != "" {
		t.Fatal("the proxy credential reached the origin")
	}
	var connect, absolute bool
	for _, s := range corp.requests() {
		if s.auth != basic(corpUser, corpPass) {
			t.Fatalf("the proxy was asked without the credentials: %+v", s)
		}
		connect = connect || s.method == http.MethodConnect && s.target == fmt.Sprintf("allowed.test:%d", sport)
		absolute = absolute || s.method == http.MethodGet && s.target == fmt.Sprintf("http://allowed.test:%d/y", pport)
	}
	if !connect || !absolute {
		t.Fatalf("the proxy saw %+v", corp.requests())
	}

	if _, err := ownGet(t, tr, ctx, fmt.Sprintf("http://denied.test:%d/", pport)); err == nil {
		t.Fatal("a refused request was sent")
	} else {
		noSecret(t, "an error", err.Error())
	}
	if corp.saw("denied.test") {
		t.Fatal("a request the policy refused reached the upstream proxy")
	}
	got := rec.all()
	if len(got) != 3 {
		t.Fatalf("%d records: %v", len(got), got)
	}
	for i, p := range got[:2] {
		if p["upstream"] != up.Authority() || p["decision"] != string(Allow) {
			t.Fatalf("record %d: %v", i, p)
		}
	}
	if _, ok := got[2]["upstream"]; ok || got[2]["decision"] != string(Deny) {
		t.Fatalf("refused record: %v", got[2])
	}
	noSecret(t, "the record", fmt.Sprint(got))
}

// Through the proxy the name is checked where it resolves here: one with an
// internal address no rule names is refused before the proxy is asked; one
// that resolves only on the proxy's side goes through.
func TestUpstreamChecksAddressesItCanResolve(t *testing.T) {
	srv, port, _ := ownServer(t)
	_ = srv
	corp := startCorp(t, "", nil)
	useUpstream(t, corp, func(u *Upstream) { u.Credentials, u.ResolveViaProxy = nil, true })
	g := ownGuard(t, Config{Rules: []Rule{
		{Host: "internal.test", Ports: []int{port}, Decision: "allow"},
		{Host: "outside.test", Ports: []int{port}, Decision: "allow"},
	}}, GuardOptions{})
	tr := &Transport{Kind: KindWebFetch, Guard: g}
	if _, err := ownGet(t, tr, context.Background(), fmt.Sprintf("http://internal.test:%d/", port)); err == nil ||
		!strings.Contains(err.Error(), "loopback") {
		t.Fatalf("internal through the proxy: %v", err)
	}
	if corp.saw("internal.test") {
		t.Fatal("an internal address reached the upstream proxy")
	}
	// fakeDNS does not know outside.test: the proxy resolves it.
	if body, err := ownGet(t, tr, context.Background(), fmt.Sprintf("http://outside.test:%d/z", port)); err != nil || body != "own ok /z" {
		t.Fatalf("a name only the proxy resolves: %q %v", body, err)
	}
}

// A no_proxy destination and loopback are reached directly.
func TestUpstreamBypass(t *testing.T) {
	_, port, hits := ownServer(t)
	corp := startCorp(t, "", nil)
	useUpstream(t, corp, func(u *Upstream) { u.Credentials, u.NoProxy = nil, []string{".corp.test", "allowed.test"} })
	g := ownGuard(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}}, GuardOptions{})
	if _, err := ownGet(t, &Transport{Kind: KindWebFetch, Guard: g}, context.Background(), fmt.Sprintf("http://allowed.test:%d/", port)); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 || len(corp.requests()) != 0 {
		t.Fatalf("a no_proxy host went through the proxy: hits %d, proxy %v", hits.Load(), corp.requests())
	}
	u := CurrentUpstream()
	for host, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "[::1]": true, "a.corp.test": true,
		"corp.test": true, "evilcorp.test": false, "allowed.test": true, "allowed.test.evil": false, "example.com": false} {
		if got := u.Bypass(host); got != want {
			t.Errorf("Bypass(%q) = %v", host, got)
		}
	}
	cidr := &Upstream{URL: corp.url("http"), NoProxy: []string{"10.0.0.0/8", "192.0.2.7", "*.svc.test"}}
	for host, want := range map[string]bool{"10.1.2.3": true, "11.0.0.1": false, "192.0.2.7": true, "a.svc.test": true, "svc.test": true} {
		if got := cidr.Bypass(host); got != want {
			t.Errorf("Bypass(%q) = %v", host, got)
		}
	}
}

// A client outside the allowlist goes through the proxy too, in place of
// the environment's, which it no longer reads.
func TestUpstreamUnguarded(t *testing.T) {
	_, port, _ := ownServer(t)
	corp := startCorp(t, basic(corpUser, corpPass), nil)
	useUpstream(t, corp, nil)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	body, err := ownGet(t, &Transport{Kind: KindRAG, Guard: Unguarded}, context.Background(), fmt.Sprintf("http://outside.test:%d/u", port))
	if err != nil || body != "own ok /u" {
		t.Fatalf("unguarded: %q %v", body, err)
	}
	if !corp.saw("outside.test") {
		t.Fatal("an unguarded request did not go through the upstream proxy")
	}
	client := UpstreamClient(5 * time.Second)
	resp, err := client.Get(fmt.Sprintf("http://outside.test:%d/v", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !corp.saw(fmt.Sprintf("outside.test:%d/v", port)) {
		t.Fatal("UpstreamClient did not go through the upstream proxy")
	}
}

// A proxy that refuses the credentials, or credentials that cannot be read,
// fail the request with no credential in the error.
func TestUpstreamCredentialFailures(t *testing.T) {
	_, port, _ := ownServer(t)
	corp := startCorp(t, basic("someone", "else"), nil)
	useUpstream(t, corp, nil)
	tr := &Transport{Kind: KindRAG, Guard: Unguarded}
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsSrv.Close)
	if _, err := ownGet(t, tr, context.Background(), fmt.Sprintf("https://outside.test:%d/", portOf(t, tlsSrv.URL))); err == nil {
		t.Fatal("a refused CONNECT succeeded")
	} else {
		noSecret(t, "an error", err.Error())
	}
	useUpstream(t, corp, func(u *Upstream) {
		u.Credentials = func() (string, string, bool, error) {
			return "", "", false, errors.New("the secret CORP_PROXY: no such secret")
		}
	})
	if _, err := ownGet(t, tr, context.Background(), fmt.Sprintf("http://outside.test:%d/", port)); err == nil ||
		!errors.Is(err, ErrUpstreamCredentials) || !strings.Contains(err.Error(), "CORP_PROXY") {
		t.Fatalf("unreadable credentials: %v", err)
	}
}

// The commands' proxy chains what its policy allowed through the corporate
// proxy: CONNECT for a tunnel, absolute form for plain http, with the
// credentials the commands never see; what it refused never reaches it.
func TestUpstreamChainsCommandsTraffic(t *testing.T) {
	var originAuth string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originAuth = r.Header.Get("Proxy-Authorization")
		_, _ = io.WriteString(w, "plain via corp")
	}))
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "tls via corp")
	}))
	t.Cleanup(secure.Close)
	pport, sport := portOf(t, plain.URL), portOf(t, secure.URL)
	corp := startCorp(t, basic(corpUser, corpPass), nil)
	up := useUpstream(t, corp, func(u *Upstream) { u.ResolveViaProxy = true })
	p, rec := startProxy(t, Config{Rules: []Rule{
		{Host: "allowed.test", Ports: []int{pport, sport}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
		{Host: "internal.test", Ports: []int{pport}, Decision: "allow"},
		{Host: "outside.test", Ports: []int{pport}, Decision: "allow"},
	}})
	tlsConf := secure.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConf.ServerName = "example.com"
	c := client(t, callURL(t, p, "cmd-1"), tlsConf)

	resp, err := c.Get(fmt.Sprintf("https://allowed.test:%d/", sport))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "tls via corp" {
		t.Fatalf("tunnel body %q", b)
	}
	resp, err = c.Get(fmt.Sprintf("http://allowed.test:%d/p", pport))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "plain via corp" || originAuth != "" {
		t.Fatalf("plain body %q, origin auth %q", b, originAuth)
	}
	for _, s := range corp.requests() {
		if s.auth != basic(corpUser, corpPass) {
			t.Fatalf("the proxy was asked without the credentials: %+v", s)
		}
	}
	if !corp.saw(fmt.Sprintf("allowed.test:%d", sport)) || !corp.saw(fmt.Sprintf("http://allowed.test:%d/p", pport)) {
		t.Fatalf("the proxy saw %+v", corp.requests())
	}

	// Refused by the policy, or by the address check: never asked of the proxy.
	for _, host := range []string{"denied.test", "internal.test"} {
		resp, err = c.Get(fmt.Sprintf("http://%s:%d/", host, pport))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || corp.saw(host) {
			t.Fatalf("%s: %d, proxy saw it %v", host, resp.StatusCode, corp.saw(host))
		}
	}
	// A name only the proxy resolves goes through.
	resp, err = c.Get(fmt.Sprintf("http://outside.test:%d/o", pport))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("outside.test: %d", resp.StatusCode)
	}
	if ev := rec.wait(t, "outside.test"); ev.Resolved != "proxy" || ev.Payload()["resolved"] != "proxy" {
		t.Fatalf("a name only the proxy resolved is not marked: %+v", ev)
	}
	p.Close()
	ev := rec.wait(t, "allowed.test")
	if ev.Upstream != up.Authority() || ev.IP != "" || ev.Decision != Allow {
		t.Fatalf("event %+v", ev)
	}
	rec.mu.Lock()
	for _, e := range rec.evs {
		noSecret(t, "the record", fmt.Sprint(e.Payload()))
	}
	rec.mu.Unlock()
}

// The corporate proxy refusing the credentials is a 502 to the command,
// naming the proxy's status, never the credentials.
func TestUpstreamRefusalReachesTheCommandWithoutCredentials(t *testing.T) {
	secure := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(secure.Close)
	sport := portOf(t, secure.URL)
	corp := startCorp(t, basic("other", "creds"), nil)
	useUpstream(t, corp, nil)
	p, rec := startProxy(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{sport}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}})
	proxyURL, _ := url.Parse(callURL(t, p, "cmd-2"))
	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	pass, _ := proxyURL.User.Password()
	fmt.Fprintf(conn, "CONNECT allowed.test:%d HTTP/1.1\r\nHost: allowed.test:%d\r\nProxy-Authorization: %s\r\n\r\n",
		sport, sport, basic(proxyURL.User.Username(), pass))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "407") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	noSecret(t, "the command's answer", string(body)+fmt.Sprint(resp.Header))
	ev := rec.wait(t, "allowed.test")
	noSecret(t, "the record", fmt.Sprint(ev.Payload()))
}

// A program Abhed starts for its own downloads reaches the proxy through a
// loopback relay that adds the credentials; it holds only the relay's
// one-time token, and no other client may use the relay.
func TestUpstreamRelay(t *testing.T) {
	_, port, _ := ownServer(t)
	corp := startCorp(t, basic(corpUser, corpPass), nil)
	useUpstream(t, corp, func(u *Upstream) { u.NoProxy = []string{"internal.corp"} })
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "relay tls") }))
	t.Cleanup(tlsSrv.Close)
	// Each listed with the port it serves on: the relay reaches no other.
	env, stop, err := UpstreamEnv([]string{fmt.Sprintf("outside.test:%d", port), fmt.Sprintf("outside.test:%d", portOf(t, tlsSrv.URL))})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var proxy, noProxy string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "HTTPS_PROXY":
			proxy = v
		case "NO_PROXY":
			noProxy = v
		}
	}
	noSecret(t, "the environment", strings.Join(env, " "))
	if !strings.Contains(noProxy, "internal.corp") || !strings.Contains(noProxy, "localhost") {
		t.Fatalf("NO_PROXY %q", noProxy)
	}
	pu, _ := url.Parse(proxy)
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := c.Get(fmt.Sprintf("http://outside.test:%d/relay", port))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "own ok /relay" || !corp.saw("/relay") {
		t.Fatalf("through the relay: %q, proxy %v", b, corp.requests())
	}
	tlsConf := tlsSrv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConf.ServerName = "example.com"
	c = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: tlsConf}}
	resp, err = c.Get(fmt.Sprintf("https://outside.test:%d/", portOf(t, tlsSrv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "relay tls" {
		t.Fatalf("CONNECT through the relay: %q", b)
	}
	// Without the token: refused, and the proxy is not asked.
	before := len(corp.requests())
	stranger := *pu
	stranger.User = url.UserPassword("abhed", "guess")
	c = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(&stranger)}}
	resp, err = c.Get(fmt.Sprintf("http://outside.test:%d/stranger", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired || len(corp.requests()) != before {
		t.Fatalf("a stranger used the relay: %d", resp.StatusCode)
	}
	stop()
	if _, err := net.DialTimeout("tcp", pu.Host, time.Second); err == nil {
		t.Fatal("the relay outlived its stop")
	}
}

// An https proxy is verified with the operator's CA bundle.
func TestUpstreamOverTLS(t *testing.T) {
	_, port, _ := ownServer(t)
	certSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certSrv.Close()
	cert := certSrv.TLS.Certificates[0]
	corp := startCorp(t, "", &tls.Config{Certificates: []tls.Certificate{cert}})
	leaf := certSrv.Certificate()
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	host := corp.url("https")
	host.Host = net.JoinHostPort("127.0.0.1", host.Port())
	SetUpstream(&Upstream{URL: host, Roots: pool, Bundle: []*x509.Certificate{leaf}})
	t.Cleanup(func() { SetUpstream(nil) })
	body, err := ownGet(t, &Transport{Kind: KindRAG, Guard: Unguarded}, context.Background(), fmt.Sprintf("http://outside.test:%d/t", port))
	if err != nil || body != "own ok /t" {
		t.Fatalf("through an https proxy: %q %v", body, err)
	}
	// Without the bundle, the proxy's certificate is not trusted.
	SetUpstream(&Upstream{URL: host})
	if _, err := ownGet(t, &Transport{Kind: KindRAG, Guard: Unguarded}, context.Background(), fmt.Sprintf("http://outside.test:%d/t", port)); err == nil {
		t.Fatal("an https proxy was trusted without the bundle")
	}
}

// slowDNS answers after the context ends, as a resolver an attacker slows.
func slowDNS(ctx context.Context, host string) ([]netip.Addr, error) {
	<-ctx.Done()
	return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
}

// failingDNS answers SERVFAIL.
func failingDNS(_ context.Context, host string) ([]netip.Addr, error) {
	return nil, &net.DNSError{Err: "server misbehaving", Name: host, IsTemporary: true}
}

// A name that does not resolve here is refused by default, before the proxy
// is asked, and so is one whose lookup times out or fails, even with
// resolve_via_proxy: only a definite not-found, for a host a rule names
// exactly, goes to the proxy with the switch on, and is marked.
func TestUpstreamResolveViaProxy(t *testing.T) {
	_, port, _ := ownServer(t)
	corp := startCorp(t, "", nil)
	rules := Config{Rules: []Rule{
		{Host: "outside.test", Ports: []int{port}, Decision: "allow"},
		{Host: "*.wild.test", Ports: []int{port}, Decision: "allow"},
	}}
	get := func(g *Guard, rec *payloads, host string) error {
		ctx := WithCaller(context.Background(), Caller{Session: "s", Record: rec.rec})
		_, err := ownGet(t, &Transport{Kind: KindWebFetch, Guard: g}, ctx, fmt.Sprintf("http://%s:%d/r", host, port))
		return err
	}
	for _, on := range []bool{false, true} {
		useUpstream(t, corp, func(u *Upstream) { u.Credentials, u.ResolveViaProxy = nil, on })
		var rec payloads
		g := ownGuard(t, rules, GuardOptions{})
		err := get(g, &rec, "outside.test")
		if on != (err == nil) {
			t.Fatalf("resolve_via_proxy %v: %v", on, err)
		}
		if !on && (!strings.Contains(err.Error(), "resolve_via_proxy") || corp.saw("outside.test")) {
			t.Fatalf("off: %v, proxy asked %v", err, corp.saw("outside.test"))
		}
		if on {
			if got := rec.all(); len(got) != 1 || got[0]["resolved"] != "proxy" {
				t.Fatalf("not marked: %v", got)
			}
		}
		// A host matched only by a wildcard is never passed unresolved.
		if err := get(g, &rec, "x.wild.test"); err == nil || corp.saw("x.wild.test") {
			t.Fatalf("a wildcard host went to the proxy unresolved: %v", err)
		}
		// Under the allowlist the client's own anchor does not count: only the rule's.
		ctx := WithCaller(context.Background(), Caller{Session: "s", Record: rec.rec})
		anch := &Transport{Kind: KindWebFetch, Guard: g, Anchored: func(string) bool { return true }}
		if _, err := ownGet(t, anch, ctx, fmt.Sprintf("http://y.wild.test:%d/r", port)); err == nil || corp.saw("y.wild.test") {
			t.Fatalf("a client anchor widened a wildcard rule: %v", err)
		}
		// A lookup that times out or fails is no not-found: refused.
		for name, dns := range map[string]func(context.Context, string) ([]netip.Addr, error){"slow": slowDNS, "servfail": failingDNS} {
			g2 := ownGuard(t, rules, GuardOptions{Resolve: dns})
			if err := get(g2, &rec, "outside.test"); err == nil {
				t.Fatalf("%s lookup, resolve_via_proxy %v: sent", name, on)
			}
		}
	}

	// The commands' proxy and an unguarded client with a check hold the same line.
	useUpstream(t, corp, func(u *Upstream) { u.Credentials = nil })
	before := len(corp.requests())
	p, _ := startProxy(t, rules)
	resp, err := client(t, callURL(t, p, "c"), nil).Get(fmt.Sprintf("http://outside.test:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(corp.requests()) != before {
		t.Fatalf("commands, switch off: %d", resp.StatusCode)
	}
	tr := &Transport{Kind: KindWebFetch, Guard: Unguarded, Check: func(string, netip.AddrPort) error { return nil }, Resolve: fakeDNS}
	if _, err := ownGet(t, tr, context.Background(), fmt.Sprintf("http://outside.test:%d/", port)); err == nil {
		t.Fatal("an unguarded web_fetch-shaped client sent an unresolved name with the switch off")
	}
	useUpstream(t, corp, func(u *Upstream) { u.Credentials, u.ResolveViaProxy = nil, true })
	if _, err := ownGet(t, tr, context.Background(), fmt.Sprintf("http://outside.test:%d/", port)); err == nil {
		t.Fatal("an unguarded client with no allowed_hosts anchor sent an unresolved name")
	}
	tr.Anchored = func(h string) bool { return h == "outside.test" }
	if _, err := ownGet(t, tr, context.Background(), fmt.Sprintf("http://outside.test:%d/a", port)); err != nil {
		t.Fatalf("an anchored host with the switch on: %v", err)
	}
	tr.Resolve = slowDNS
	if _, err := ownGet(t, tr, context.Background(), fmt.Sprintf("http://outside.test:%d/", port)); err == nil {
		t.Fatal("a timed-out lookup was sent")
	}
}

// The relay reaches the hosts it was started for and nothing else, without
// asking the proxy.
func TestUpstreamRelayRefusesOtherHosts(t *testing.T) {
	_, port, _ := ownServer(t)
	corp := startCorp(t, "", nil)
	useUpstream(t, corp, func(u *Upstream) { u.Credentials = nil })
	proxy, _, stop, err := UpstreamRelay([]string{"proxy.golang.test"})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	pu, _ := url.Parse(proxy)
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := c.Get(fmt.Sprintf("http://other.test:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(corp.requests()) != 0 {
		t.Fatalf("an unlisted host through the relay: %d, proxy %v", resp.StatusCode, corp.requests())
	}
	conn, err := net.Dial("tcp", pu.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	pass, _ := pu.User.Password()
	fmt.Fprintf(conn, "CONNECT other.test:443 HTTP/1.1\r\nHost: other.test:443\r\nProxy-Authorization: %s\r\n\r\n", basic(pu.User.Username(), pass))
	r, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusForbidden || len(corp.requests()) != 0 {
		t.Fatalf("an unlisted CONNECT through the relay: %d", r.StatusCode)
	}
}

// A listed host is reached on its own ports only: 443 by CONNECT and 80 in
// absolute form for a host alone, or the port listed with it.
func TestUpstreamRelayRefusesOtherPorts(t *testing.T) {
	corp := startCorp(t, "", nil)
	useUpstream(t, corp, func(u *Upstream) { u.Credentials = nil })
	proxy, _, stop, err := UpstreamRelay([]string{"proxy.golang.test", "sum.golang.test:8443"})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	pu, _ := url.Parse(proxy)
	pass, _ := pu.User.Password()
	ask := func(line string) int {
		t.Helper()
		conn, err := net.Dial("tcp", pu.Host)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		method, target, _ := strings.Cut(line, " ")
		host := target
		if u, err := url.Parse(target); err == nil && u.Host != "" {
			host = u.Host
		}
		fmt.Fprintf(conn, "%s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", line, host, basic(pu.User.Username(), pass))
		r, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		return r.StatusCode
	}
	for _, line := range []string{
		"CONNECT proxy.golang.test:22",
		"CONNECT proxy.golang.test:80",
		"GET http://proxy.golang.test:8080/",
		"GET http://proxy.golang.test:443/",
		"CONNECT sum.golang.test:443",
		"GET http://sum.golang.test/",
	} {
		if code := ask(line); code != http.StatusForbidden || len(corp.requests()) != 0 {
			t.Fatalf("%s: %d, proxy %v", line, code, corp.requests())
		}
	}
	// The listed ports pass the relay's check and reach the proxy.
	for _, line := range []string{"CONNECT proxy.golang.test:443", "CONNECT sum.golang.test:8443"} {
		if code := ask(line); code == http.StatusForbidden {
			t.Fatalf("%s: refused", line)
		}
	}
	if !corp.saw("proxy.golang.test:443") || !corp.saw("sum.golang.test:8443") {
		t.Fatalf("the proxy was not asked: %v", corp.requests())
	}
}

// A destination the proxy is bypassed for is reached with the transport as
// it was: not through the proxy, and not trusting the proxy's CA bundle.
func TestUpstreamBypassedTrustsNoBundle(t *testing.T) {
	bundleSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(bundleSrv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(bundleSrv.Certificate())
	corp := startCorp(t, "", nil)
	useUpstream(t, corp, func(u *Upstream) {
		u.Credentials, u.Roots, u.Bundle, u.NoProxy = nil, pool, []*x509.Certificate{bundleSrv.Certificate()}, []string{"direct.test"}
	})
	// A client whose own roots do not hold the bundle's certificate.
	tr := &Transport{Kind: KindK8s, Guard: Unguarded, Base: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "example.com"},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, strings.TrimPrefix(bundleSrv.URL, "https://"))
		}}}
	if _, err := ownGet(t, tr, context.Background(), "https://direct.test/"); err == nil {
		t.Fatal("a bypassed destination trusted the proxy's CA bundle")
	}
	if len(corp.requests()) != 0 {
		t.Fatal("a bypassed destination went through the proxy")
	}
}

// A CONNECT authority with a line break or a space is refused, so no caller
// can add a header to the proxy request.
func TestUpstreamTunnelAuthority(t *testing.T) {
	corp := startCorp(t, "", nil)
	u := useUpstream(t, corp, nil)
	for _, a := range []string{"", "host:443\r\nX-Evil: 1", "host :443", "host:443\n"} {
		if _, err := u.DialTunnel(context.Background(), a, time.Second); err == nil {
			t.Errorf("%q was tunnelled", a)
		}
	}
	if len(corp.requests()) != 0 {
		t.Fatal("the proxy was asked")
	}
}
