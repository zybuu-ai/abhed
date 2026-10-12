package egress

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// With inspect_tls on, a path-narrowed rule allows that path in the tunnel and
// denies the rest; off, the same rule cannot allow the tunnel.
func TestInspectedTunnelIsJudgedByPath(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "secret-ok "+r.URL.Path)
	}))
	defer upstream.Close()
	port := portOf(t, upstream.URL)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	cfg := Config{InspectTLS: true, Rules: []Rule{{Host: "127.0.0.1", Ports: []int{port}, Paths: []string{"/ok"}, Methods: []string{"GET"},
		Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}}
	rec := &recorder{}
	p, err := Start(Options{Policy: mustCompile(t, cfg), Record: rec.add, Resolve: fakeDNS, DialTimeout: 2 * time.Second, UpstreamRoots: roots})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	ca := x509.NewCertPool()
	if !ca.AppendCertsFromPEM(p.CA()) {
		t.Fatal("no CA")
	}
	u, _ := url.Parse(callURL(t, p, "call-i"))
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: &tls.Config{RootCAs: ca}}}
	get := func(path string) (int, string) {
		resp, err := c.Get(fmt.Sprintf("https://127.0.0.1:%d%s", port, path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/ok"); code != 200 || body != "secret-ok /ok" {
		t.Fatalf("/ok: %d %q", code, body)
	}
	if code, body := get("/exfil"); code != 403 || strings.Contains(body, "secret-ok") {
		t.Fatalf("/exfil: %d %q", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	var ok, denied bool
	for time.Now().Before(deadline) && (!ok || !denied) {
		rec.mu.Lock()
		for _, e := range rec.evs {
			ok = ok || e.Kind == "https" && e.Path == "/ok" && e.Decision == Allow && e.Method == "GET" && e.CallID == "call-i"
			denied = denied || e.Kind == "https" && e.Path == "/exfil" && e.Decision == Deny
		}
		rec.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if !ok || !denied {
		t.Fatalf("records: %+v", rec.evs)
	}
	// A client that does not trust the session's CA fails its handshake.
	plain := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
	if resp, err := plain.Get(fmt.Sprintf("https://127.0.0.1:%d/ok", port)); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a client without the session's CA was served")
	}

	// Off: a rule narrowed by path cannot allow a tunnel.
	cfg.InspectTLS = false
	p2, err := Start(Options{Policy: mustCompile(t, cfg), Resolve: fakeDNS, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p2.Close() }()
	u2, _ := url.Parse(callURL(t, p2, "call-j"))
	c2 := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(u2), TLSClientConfig: &tls.Config{RootCAs: roots}}}
	if resp, err := c2.Get(fmt.Sprintf("https://127.0.0.1:%d/ok", port)); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a tunnel was allowed by a rule narrowed by path without inspection")
	}
	if p2.CA() != nil {
		t.Fatal("a proxy that does not inspect has a CA")
	}
}

// A request inside an inspected tunnel that names another host is refused.
func TestInspectedTunnelRefusesAnotherHost(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "reached") }))
	defer upstream.Close()
	port := portOf(t, upstream.URL)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	cfg := Config{InspectTLS: true, Rules: []Rule{{Host: "127.0.0.1", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}}
	p, err := Start(Options{Policy: mustCompile(t, cfg), Resolve: fakeDNS, DialTimeout: 2 * time.Second, UpstreamRoots: roots})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	ca := x509.NewCertPool()
	ca.AppendCertsFromPEM(p.CA())
	u, _ := url.Parse(callURL(t, p, "call-h"))
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: &tls.Config{RootCAs: ca}}}
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://127.0.0.1:%d/", port), nil)
	req.Host = "elsewhere.example"
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 403 || strings.Contains(string(b), "reached") {
		t.Fatalf("another host: %d %s", resp.StatusCode, b)
	}
}

// openTunnel sends a CONNECT for target through p as call and returns the
// connection once the proxy answers 200.
func openTunnel(t *testing.T, p *Proxy, call, target string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c := issue(t, p, call)
	conn, err := net.DialTimeout("tcp", p.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte(call + ":" + c.token))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target, target, auth); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	_ = resp.Body.Close() // a 200 to CONNECT has no body; the tunnel follows
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s: %s", target, resp.Status)
	}
	return conn, br
}

func inspecting(t *testing.T, rules []Rule, roots *x509.CertPool) (*Proxy, *recorder) {
	t.Helper()
	rec := &recorder{}
	p, err := Start(Options{Policy: mustCompile(t, Config{InspectTLS: true, Rules: rules}), Record: rec.add, Resolve: fakeDNS,
		DialTimeout: 2 * time.Second, UpstreamRoots: roots})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, rec
}

// Under inspection a tunnel that does not carry TLS is judged again by host
// and port: a rule narrowed by path opened it, and does not let it through.
func TestInspectedTunnelWithoutTLSIsJudgedByHostAndPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan int, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
				b, _ := io.ReadAll(c)
				got <- len(b)
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	target := fmt.Sprintf("127.0.0.1:%d", port)
	for _, tc := range []struct {
		name    string
		paths   []string
		through bool
	}{{"narrowed", []string{"/ok"}, false}, {"whole host", nil, true}} {
		p, rec := inspecting(t, []Rule{{Host: "127.0.0.1", Ports: []int{port}, Paths: tc.paths, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}, nil)
		conn, br := openTunnel(t, p, "call-ssh", target)
		if _, err := io.WriteString(conn, "SSH-2.0-OpenSSH_9.6\r\n"); err != nil {
			t.Fatal(err)
		}
		closeWrite(conn)
		_, _ = io.ReadAll(br)
		_ = conn.Close()
		var n int
		select {
		case n = <-got:
		case <-time.After(5 * time.Second):
		}
		if passed := n > 0; passed != tc.through {
			t.Fatalf("%s: %d bytes reached the server, want through=%v", tc.name, n, tc.through)
		}
		if !tc.through {
			e := rec.wait(t, "127.0.0.1")
			if e.Decision != Deny || !strings.Contains(e.Reason, "does not carry TLS") {
				t.Fatalf("%s: recorded %+v", tc.name, e)
			}
		}
	}
}

// Under inspection a TLS name (SNI) other than the tunnel's host fails the
// handshake, even for a client that would accept any certificate.
func TestInspectedTunnelRefusesAnotherTLSName(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "reached") }))
	t.Cleanup(upstream.Close) // after the proxy and the client, which a failure leaves open
	port := portOf(t, upstream.URL)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	p, rec := inspecting(t, []Rule{{Host: "127.0.0.1", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}, roots)
	conn, br := openTunnel(t, p, "call-sni", fmt.Sprintf("127.0.0.1:%d", port))
	tc := tls.Client(&prefixed{Conn: conn, r: br}, &tls.Config{ServerName: "elsewhere.example", InsecureSkipVerify: true}) // #nosec G402 -- the proxy must refuse even so
	if err := tc.Handshake(); err == nil {
		t.Fatal("a handshake naming another host was served")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		for _, e := range rec.evs {
			if e.Rule == "tls" && e.Decision == Deny && strings.Contains(e.Reason, "elsewhere.example") {
				rec.mu.Unlock()
				return
			}
		}
		rec.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no tls refusal recorded: %+v", rec.evs)
}

// A protocol upgrade inside an inspected tunnel is refused with 403 and
// never reaches the server.
func TestInspectedTunnelRefusesAnUpgrade(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "reached") }))
	t.Cleanup(upstream.Close) // after the proxy and the client, which a failure leaves open
	port := portOf(t, upstream.URL)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	p, _ := inspecting(t, []Rule{{Host: "127.0.0.1", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}, roots)
	ca := x509.NewCertPool()
	ca.AppendCertsFromPEM(p.CA())
	u, _ := url.Parse(callURL(t, p, "call-up"))
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: &tls.Config{RootCAs: ca}}}
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://127.0.0.1:%d/ws", port), nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || strings.Contains(string(b), "reached") {
		t.Fatalf("an upgrade: %d %s", resp.StatusCode, b)
	}
}

// A client that waits for 100 Continue before its body is answered by the
// proxy once the request is allowed, and its upload goes through.
func TestInspectedTunnelAnswersExpectContinue(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "got "+string(b))
	}))
	t.Cleanup(upstream.Close) // after the proxy and the client, which a failure leaves open
	port := portOf(t, upstream.URL)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	p, _ := inspecting(t, []Rule{{Host: "127.0.0.1", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}, roots)
	ca := x509.NewCertPool()
	ca.AppendCertsFromPEM(p.CA())
	conn, br := openTunnel(t, p, "call-100", fmt.Sprintf("127.0.0.1:%d", port))
	tc := tls.Client(&prefixed{Conn: conn, r: br}, &tls.Config{ServerName: "127.0.0.1", RootCAs: ca})
	if _, err := fmt.Fprintf(tc, "POST /up HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nContent-Length: 5\r\nExpect: 100-continue\r\n\r\n", port); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	tr := bufio.NewReader(tc)
	line, err := tr.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 100") {
		t.Fatalf("no 100 Continue before the body: %q %v", line, err)
	}
	if _, err := tr.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tc, "hello"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "got hello" {
		t.Fatalf("the upload: %d %q", resp.StatusCode, b)
	}
}
