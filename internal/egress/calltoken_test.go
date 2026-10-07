package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// basicFor is a Proxy-Authorization value with user and c's token.
func basicFor(user string, c *Call) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+c.token))
}

func connectAs(t *testing.T, p *Proxy, auth, path string) string {
	t.Helper()
	return send(t, p, fmt.Sprintf("GET http://denied.test%s HTTP/1.1\r\nHost: denied.test\r\nProxy-Authorization: %s\r\n\r\n", path, auth))
}

// The call id is the one the credential was issued for: a user name naming
// another call of the session changes nothing.
func TestCallIDComesFromTheCredential(t *testing.T) {
	p, rec := startProxy(t, Config{})
	a, _ := issue(t, p, "call-a"), issue(t, p, "call-b")
	if st := connectAs(t, p, basicFor("call-b", a), "/forged"); !strings.Contains(st, " 403 ") {
		t.Fatalf("forged user name: %s", st)
	}
	e := rec.waitFor(t, "the forged request", func(e Event) bool { return e.Path == "/forged" })
	if e.CallID != "call-a" {
		t.Fatalf("recorded under %q, want the credential's call-a", e.CallID)
	}
}

// After its call ends a credential is refused, by the proxy and the
// resolver, and the refusal is recorded under the ended call.
func TestEndedCallIsRefused(t *testing.T) {
	p, rec, sock := dnsProxy(t, Config{Rules: []Rule{{Host: "allowed.test", Decision: "allow"}}}, Options{})
	c := issue(t, p, "call-gone")
	if st := connectAs(t, p, basicFor("x", c), "/live"); !strings.Contains(st, " 403 ") {
		t.Fatalf("live credential: %s", st)
	}
	c.End()
	if st := connectAs(t, p, basicFor("x", c), "/late"); !strings.Contains(st, " 407 ") {
		t.Fatalf("ended credential: %s", st)
	}
	e := rec.waitFor(t, "the late request", func(e Event) bool { return e.Kind == "auth" })
	if e.CallID != "call-gone" || e.Decision != Deny || !strings.Contains(e.Reason, "ended") {
		t.Fatalf("late request recorded as %+v", e)
	}
	out, err := exchangeDNS(sock, Credential(c.URL()), query(1, "allowed.test", dnsTypeA))
	if err != nil {
		t.Fatal(err)
	}
	if r := parseReply(t, out); r.rcode != rcodeRefused || len(r.answers) != 0 {
		t.Fatalf("lookup with an ended credential: %+v", r)
	}
	d := rec.waitFor(t, "the late lookup", func(e Event) bool { return e.Kind == "dns" && e.Rule == "auth" })
	if d.CallID != "call-gone" {
		t.Fatalf("late lookup recorded as %+v", d)
	}
	// A credential never issued names no call.
	forged := &Call{p: p, id: "call-gone", token: strings.Repeat("0", 48)}
	if st := connectAs(t, p, basicFor("call-gone", forged), "/forged"); !strings.Contains(st, " 407 ") {
		t.Fatalf("unknown token: %s", st)
	}
}

// Ending a call closes the tunnels opened with its credential.
func TestEndClosesTheCallsConnections(t *testing.T) {
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
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	p, _ := startProxy(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}})
	call := issue(t, p, "call-t")
	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	fmt.Fprintf(c, "CONNECT allowed.test:%d HTTP/1.1\r\nProxy-Authorization: %s\r\n\r\n", port, basicFor("u", call))
	buf := make([]byte, 64)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, _ := c.Read(buf); !strings.Contains(string(buf[:n]), " 200 ") {
		t.Fatalf("CONNECT: %q", buf[:n])
	}
	call.End()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ne net.Error
	if _, err := c.Read(buf); err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatal("the tunnel stayed open after its call ended")
	}
}

// Ended credentials are remembered only so far; the oldest then name no call.
func TestEndedCallsAreBounded(t *testing.T) {
	p, rec := startProxy(t, Config{})
	first := issue(t, p, "call-0")
	first.End()
	for i := range maxEnded {
		issue(t, p, fmt.Sprintf("call-%d", i+1)).End()
	}
	p.mu.Lock()
	n := len(p.creds)
	p.mu.Unlock()
	if n != maxEnded {
		t.Fatalf("%d credentials held, want %d", n, maxEnded)
	}
	_ = connectAs(t, p, basicFor("call-0", first), "/old")
	if e := rec.waitFor(t, "the oldest", func(e Event) bool { return e.Kind == "auth" }); e.CallID != "" {
		t.Fatalf("a forgotten credential was attributed to %q", e.CallID)
	}
}

// Issuing, using and ending credentials at once is safe, and no request is
// ever recorded under a call other than its credential's.
func TestCredentialsConcurrently(t *testing.T) {
	p, rec := startProxy(t, Config{})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 5 {
				id := fmt.Sprintf("c%d-%d", i, j)
				c, err := p.Issue(id)
				if err != nil {
					t.Error(err)
					return
				}
				_ = connectAs(t, p, basicFor("someone-else", c), "/"+id)
				go c.End()
				_ = connectAs(t, p, basicFor("someone-else", c), "/"+id+"-late")
			}
		}()
	}
	wg.Wait()
	for _, e := range rec.events(func(e Event) bool { return e.Path != "" }) {
		if want := strings.TrimSuffix(strings.TrimPrefix(e.Path, "/"), "-late"); e.CallID != want {
			t.Fatalf("%s recorded under %q", e.Path, e.CallID)
		}
	}
}

// A request admitted while its call was live is stopped when the call ends:
// a dial under way is cancelled, and the upstream is closed.
func TestEndCancelsADialUnderWay(t *testing.T) {
	started := make(chan struct{})
	block := func(ctx context.Context, host string) ([]netip.Addr, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p, rec := startWith(t, Config{Rules: []Rule{{Host: "slow.test", Decision: "allow"}}}, Options{Resolve: block, DialTimeout: time.Minute})
	call := issue(t, p, "call-slow")
	done := make(chan string, 1)
	go func() {
		done <- connectAsHost(t, p, basicFor("u", call), "slow.test")
	}()
	<-started
	call.End()
	select {
	case st := <-done:
		if strings.Contains(st, " 200 ") || strings.Contains(st, " 502 ") {
			t.Fatalf("a request whose call ended mid-dial got %q", st)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the dial was not cancelled when its call ended")
	}
	e := rec.waitFor(t, "the cancelled request", func(e Event) bool { return e.Host == "slow.test" })
	if e.Decision != Deny || !strings.Contains(e.Reason, "ended") || e.CallID != "call-slow" {
		t.Fatalf("recorded %+v", e)
	}
}

// connectAsHost sends a CONNECT for host:443 and returns the status line.
func connectAsHost(t *testing.T, p *Proxy, auth, host string) string {
	t.Helper()
	return send(t, p, fmt.Sprintf("CONNECT %s:443 HTTP/1.1\r\nProxy-Authorization: %s\r\n\r\n", host, auth))
}

// Ending a call closes the upstream of a plain request still waiting on its
// response, which closing the client's side alone does not reach.
func TestEndClosesTheUpstream(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	got, closed := make(chan struct{}), make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		r := bufio.NewReader(c)
		for {
			l, err := r.ReadString('\n')
			if err != nil || l == "\r\n" {
				break
			}
		}
		close(got)
		_, _ = io.Copy(io.Discard, r) // never answers; returns when the proxy closes
		close(closed)
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	p, _ := startProxy(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}})
	call := issue(t, p, "call-up")
	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	fmt.Fprintf(c, "GET http://allowed.test:%d/wait HTTP/1.1\r\nHost: allowed.test\r\nProxy-Authorization: %s\r\n\r\n", port, basicFor("u", call))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the upstream")
	}
	call.End()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream stayed open after the call ended")
	}
}
