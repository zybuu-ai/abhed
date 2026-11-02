// Package upstreamtest is a stand-in corporate proxy for tests: it answers
// CONNECT and absolute-form requests on loopback, records what it was asked
// for, and reaches every name it is asked for on 127.0.0.1.
package upstreamtest

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Proxy is a running stand-in proxy.
type Proxy struct {
	ln net.Listener
	mu sync.Mutex
	// seen are the request targets it was asked for: authority for CONNECT, URL otherwise.
	seen []string
	auth []string
}

// Start starts a proxy, closed when the test ends.
func Start(t testing.TB) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{ln: ln}
	go p.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

// Use puts p in force as the upstream proxy with user:pass, until the test ends.
func (p *Proxy) Use(t testing.TB, user, pass string) *egress.Upstream {
	t.Helper()
	u := &egress.Upstream{URL: &url.URL{Scheme: "http", Host: p.ln.Addr().String()}}
	if user != "" {
		u.Credentials = func() (string, string, bool, error) { return user, pass, true, nil }
	}
	egress.SetUpstream(u)
	t.Cleanup(func() { egress.SetUpstream(nil) })
	return u
}

// Saw reports whether any target it was asked for contains s.
func (p *Proxy) Saw(s string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.seen {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// Auth are the Proxy-Authorization values it was sent, in order.
func (p *Proxy) Auth() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.auth...)
}

func (p *Proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func loopback(authority string) string {
	_, port, err := net.SplitHostPort(authority)
	if err != nil {
		port = "80"
	}
	return net.JoinHostPort("127.0.0.1", port)
}

func (p *Proxy) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.seen = append(p.seen, req.RequestURI)
	p.auth = append(p.auth, req.Header.Get("Proxy-Authorization"))
	p.mu.Unlock()
	if req.Method == http.MethodConnect {
		up, err := net.Dial("tcp", loopback(req.Host))
		if err != nil {
			_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}
		defer func() { _ = up.Close() }()
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
		go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
		<-done
		return
	}
	u, err := url.Parse(req.RequestURI)
	if err != nil || !u.IsAbs() {
		_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
		return
	}
	up, err := net.Dial("tcp", loopback(u.Host))
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer func() { _ = up.Close() }()
	req.Header.Del("Proxy-Authorization")
	req.RequestURI, req.Close = "", true
	if err := req.Write(up); err != nil {
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(up), req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_ = resp.Write(c)
}
