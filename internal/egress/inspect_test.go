package egress

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// With inspect_tls on, a command's HTTPS is judged request by request: a
// rule narrowed to a path allows that path inside the tunnel and denies the
// rest, each recorded with its method and path. Off, the same rule cannot
// allow the tunnel at all.
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
