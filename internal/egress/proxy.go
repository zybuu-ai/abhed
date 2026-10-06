package egress

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one decision, as the record keeps it: never a body, a header
// value or a credential.
type Event struct {
	CallID   string
	Kind     string // "connect" or "http"
	Host     string
	Port     uint16
	IP       string
	Method   string
	Path     string
	Decision Decision
	Rule     string
	Reason   string
	BytesIn  int64
	BytesOut int64
	// Repeats, on a summary, is how many decisions like this one were not
	// recorded one by one in the interval it covers.
	Repeats int64
}

// EventName is the record's name for an Event.
const EventName = "egress.decision"

// Payload is the event as a record payload.
func (e Event) Payload() map[string]any {
	m := map[string]any{
		"call_id": e.CallID, "kind": e.Kind, "host": e.Host, "port": int(e.Port),
		"decision": string(e.Decision), "rule": e.Rule, "reason": e.Reason,
		"bytes_in": e.BytesIn, "bytes_out": e.BytesOut,
	}
	if e.IP != "" {
		m["ip"] = e.IP
	}
	if e.Kind == "http" {
		m["method"], m["path"] = e.Method, e.Path
	}
	if e.Repeats > 0 {
		m["repeats"] = e.Repeats
	}
	return m
}

// Options configure a Proxy.
type Options struct {
	Policy *Policy
	// Record receives one Event per connection or request; it must not block long.
	Record func(Event)
	// Resolve looks a name up; nil uses the system resolver. Tests replace it.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// DialTimeout bounds each upstream dial; zero means 10 seconds.
	DialTimeout time.Duration
	// IPv6Loopback listens on [::1] at the same port as well, for a sandbox
	// that can allow only "localhost", which is both addresses: no other
	// process may hold the IPv6 half of the port.
	IPv6Loopback bool
	// Idle overrides the policy's idle time; zero keeps it.
	Idle time.Duration
	// MaxConns bounds the connections served at once; zero means 256.
	MaxConns int
	// Burst and Interval rate-limit denials in the record: the first Burst
	// of a kind in each Interval are recorded one by one, then counted and
	// recorded as one summary at the interval's end. Zero means 10 and a
	// minute.
	Burst    int
	Interval time.Duration
}

// Timeouts and bounds.
const (
	headerTimeout = 30 * time.Second
	maxConns      = 256
	maxHeader     = 64 << 10
)

// Proxy is one session's forward proxy.
type Proxy struct {
	opts  Options
	token string
	tcp   net.Listener
	addr  netip.AddrPort

	mu        sync.Mutex
	listeners []net.Listener
	conns     map[net.Conn]struct{}
	closed    bool

	slots chan struct{}
	wg    sync.WaitGroup
	once  sync.Once
	limit *limiter
	// served counts connections handled, for tests and doctor.
	served atomic.Int64
}

// Start listens on a random loopback port and serves until Close.
func Start(opts Options) (*Proxy, error) {
	if opts.Policy == nil {
		return nil, errors.New("egress: no policy")
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 10 * time.Second
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = maxConns
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	lns, err := listenLoopback(opts.IPv6Loopback)
	if err != nil {
		return nil, err
	}
	p := &Proxy{opts: opts, token: hex.EncodeToString(b[:]), tcp: lns[0], conns: map[net.Conn]struct{}{},
		slots: make(chan struct{}, opts.MaxConns)}
	p.limit = newLimiter(opts.Burst, opts.Interval, p.emit)
	p.addr = lns[0].Addr().(*net.TCPAddr).AddrPort()
	for _, ln := range lns {
		p.serve(ln)
	}
	return p, nil
}

// listenLoopback takes a random port on 127.0.0.1 and, when asked and the
// host has IPv6 loopback, the same port on [::1]; a port whose IPv6 half is
// taken is given back and another tried.
func listenLoopback(v6 bool) ([]net.Listener, error) {
	for range 20 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("egress: listening on loopback: %w", err)
		}
		if !v6 {
			return []net.Listener{ln}, nil
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln6, err := net.Listen("tcp", net.JoinHostPort("::1", fmt.Sprint(port)))
		if err == nil {
			return []net.Listener{ln, ln6}, nil
		}
		_ = ln.Close()
		if !ipv6Loopback() {
			// No IPv6 loopback here: no process can hold [::1] either.
			return listenLoopback(false)
		}
	}
	return nil, errors.New("egress: no loopback port free on both 127.0.0.1 and [::1]")
}

// ipv6Loopback reports whether [::1] can be listened on at all.
func ipv6Loopback() bool {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// ListenUnix serves the proxy on a unix socket as well, for a relay inside
// a sandbox whose network namespace cannot reach host loopback.
func (p *Proxy) ListenUnix(path string) error {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	p.serve(ln)
	return nil
}

func (p *Proxy) serve(ln net.Listener) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = ln.Close()
		return
	}
	p.listeners = append(p.listeners, ln)
	p.wg.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case p.slots <- struct{}{}:
			default:
				_ = c.Close() // over the bound: refused, never queued
				p.record(Event{Kind: "request", Decision: Deny, Rule: "cap",
					Reason: fmt.Sprintf("refused before it was read: %d connections are open, the most one session's proxy serves", p.opts.MaxConns)})
				continue
			}
			if !p.track(c) {
				<-p.slots
				_ = c.Close()
				continue
			}
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer func() { <-p.slots }()
				defer p.untrack(c)
				p.served.Add(1)
				p.handle(c)
			}()
		}
	}()
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	_ = c.Close()
}

// Addr is the loopback address the proxy listens on.
func (p *Proxy) Addr() netip.AddrPort { return p.addr }

// Token is the session's proxy password.
func (p *Proxy) Token() string { return p.token }

// URL is the proxy URL a command is given: the call id as the user name,
// the session token as the password.
func (p *Proxy) URL(callID string) string {
	if callID == "" {
		callID = "abhed"
	}
	u := url.URL{Scheme: "http", User: url.UserPassword(callID, p.token), Host: p.addr.String()}
	return u.String()
}

// Env is the environment that sends a command's HTTP clients through the
// proxy, in both cases since clients read one or the other. Loopback is
// left direct: on the sandbox tiers it is the sandbox's own.
func (p *Proxy) Env(callID string) []string {
	u := p.URL(callID)
	const noProxy = "localhost,127.0.0.1,::1"
	return []string{
		"HTTP_PROXY=" + u, "http_proxy=" + u,
		"HTTPS_PROXY=" + u, "https_proxy=" + u,
		"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
	}
}

// Served is how many connections the proxy has handled.
func (p *Proxy) Served() int64 { return p.served.Load() }

// Close stops listening, ends open connections, waits for them, and
// records the summaries of denials still being counted.
func (p *Proxy) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		for _, ln := range p.listeners {
			_ = ln.Close()
		}
		for c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
		p.limit.stop()
	})
	return nil
}

// record passes e to the limiter, which records it now, or counts it into
// a summary.
func (p *Proxy) record(e Event) {
	if !p.opts.Policy.RecordPaths() {
		e.Path = ""
	}
	p.limit.record(e)
}

func (p *Proxy) emit(e Event) {
	if p.opts.Record != nil {
		p.opts.Record(e)
	}
}

// handle serves one client connection: one request, then it is closed.
func (p *Proxy) handle(raw net.Conn) {
	act := &activity{}
	c := &active{Conn: raw, act: act}
	_ = c.SetReadDeadline(time.Now().Add(headerTimeout))
	lim := &headerLimit{r: c, left: maxHeader}
	br := bufio.NewReaderSize(lim, 4096)
	req, err := http.ReadRequest(br)
	lim.lift()
	if err != nil {
		if !errors.Is(err, io.EOF) {
			p.record(Event{Kind: "request", Decision: Deny, Rule: "parse", Reason: "refused before it was read: " + headError(err)})
		}
		writeStatus(c, http.StatusBadRequest, "abhed egress: malformed request\n", nil)
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	callID, ok := p.authorize(req.Header.Get("Proxy-Authorization"))
	if !ok {
		p.record(Event{Kind: "auth", Decision: Deny, Rule: "auth", Reason: "missing or wrong proxy credentials"})
		writeStatus(c, http.StatusProxyAuthRequired, "abhed egress: proxy credentials required\n",
			http.Header{"Proxy-Authenticate": {`Basic realm="abhed"`}})
		return
	}
	t, err := ParseTarget(req.Method, req.RequestURI)
	if err != nil {
		p.record(Event{CallID: callID, Kind: kindOf(req.Method), Decision: Deny, Rule: "parse", Reason: err.Error()})
		writeStatus(c, http.StatusBadRequest, "abhed egress: "+err.Error()+"\n", nil)
		return
	}
	ev := Event{CallID: callID, Kind: kindOf(t.Method), Host: t.Host, Port: t.Port}
	if !t.Tunnel {
		ev.Method, ev.Path = t.Method, t.Path
	}
	v := p.opts.Policy.Decide(t.Request())
	ev.Decision, ev.Rule, ev.Reason = v.Decision, v.Rule, v.Reason
	if !v.Allowed() {
		p.record(ev)
		writeStatus(c, http.StatusForbidden, fmt.Sprintf("abhed egress: %s refused by %s: %s\n", t.Authority(), v.Rule, v.Reason), nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.opts.DialTimeout)
	up, ip, err := p.dial(ctx, t, v)
	cancel()
	ev.IP = ip
	if err != nil {
		var ref *refusal
		if errors.As(err, &ref) {
			ev.Decision, ev.Reason = Deny, ref.Error()
			p.record(ev)
			writeStatus(c, http.StatusForbidden, "abhed egress: "+ref.Error()+"\n", nil)
			return
		}
		ev.Reason = v.Reason + "; dial failed: " + err.Error()
		p.record(ev)
		writeStatus(c, http.StatusBadGateway, "abhed egress: could not reach "+t.Authority()+"\n", nil)
		return
	}
	cu := &counted{Conn: up, act: act}
	if !p.track(cu) {
		_ = cu.Close()
		return
	}
	idle := p.opts.Idle
	if idle <= 0 {
		idle = p.opts.Policy.Idle()
	}
	act.touch()
	idled := watchIdle(idle, act, c, cu)
	defer func() {
		p.untrack(cu)
		_ = cu.Close()
		if idled() {
			ev.Reason += fmt.Sprintf("; closed after %s with no bytes either way", idle)
		}
		ev.BytesIn, ev.BytesOut = cu.in.Load(), cu.out.Load()
		p.record(ev)
	}()
	if t.Tunnel {
		p.tunnel(c, br, cu)
		return
	}
	p.forward(c, req, t, cu)
}

func kindOf(method string) string {
	if method == "CONNECT" {
		return "connect"
	}
	return "http"
}

// authorize checks the Basic credentials: any user name, which is the call
// id the launcher set, and the session token as the password.
func (p *Proxy) authorize(h string) (string, bool) {
	scheme, enc, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return "", false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(p.token)) != 1 {
		return "", false
	}
	if u, err := url.PathUnescape(user); err == nil {
		user = u
	}
	if len(user) > 128 || strings.ContainsFunc(user, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		user = "invalid"
	}
	return user, true
}

// refusal is an address the policy does not reach, kept apart from a
// network failure.
type refusal struct {
	host string
	addr netip.Addr
	why  string
}

func (e *refusal) Error() string {
	if e.host != e.addr.String() {
		return fmt.Sprintf("%s resolves to %s, %s, which no rule names in allow_ips", e.host, e.addr, e.why)
	}
	return fmt.Sprintf("%s is %s, which no rule names in allow_ips", e.addr, e.why)
}

// dial resolves the host, checks every address, and connects to one it
// checked: the name is never resolved again.
func (p *Proxy) dial(ctx context.Context, t Target, v Verdict) (net.Conn, string, error) {
	addrs, err := p.resolve(ctx, t.Host)
	if err != nil {
		return nil, "", err
	}
	if len(addrs) == 0 {
		return nil, "", fmt.Errorf("%s has no address", t.Host)
	}
	// A name that mixes public and internal addresses is refused whole.
	for _, a := range addrs {
		a = a.Unmap()
		if ap := netip.AddrPortFrom(a, t.Port); p.self(ap) {
			return nil, a.String(), &refusal{host: t.Host, addr: a, why: "this proxy itself"}
		}
		if named(v.AllowIPs, a) {
			continue
		}
		if why := AddrRefusal(a); why != "" {
			return nil, a.String(), &refusal{host: t.Host, addr: a, why: why}
		}
	}
	d := net.Dialer{}
	var last error
	for _, a := range addrs {
		ap := netip.AddrPortFrom(a.Unmap(), t.Port)
		c, err := d.DialContext(ctx, "tcp", ap.String())
		if err == nil {
			return c, ap.Addr().String(), nil
		}
		last = err
	}
	return nil, addrs[0].Unmap().String(), last
}

func (p *Proxy) self(ap netip.AddrPort) bool {
	return ap.Port() == p.addr.Port() && (ap.Addr() == p.addr.Addr() || ap.Addr().IsLoopback() || ap.Addr().IsUnspecified())
}

func named(list []netip.Prefix, a netip.Addr) bool {
	for _, pf := range list {
		if pf.Contains(a) {
			return true
		}
	}
	return false
}

func (p *Proxy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, nil
	}
	if p.opts.Resolve != nil {
		return p.opts.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// tunnel answers a CONNECT and copies both ways until either side ends.
func (p *Proxy) tunnel(c net.Conn, br *bufio.Reader, up *counted) {
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		// What the client sent after the request line, then the rest.
		if n := br.Buffered(); n > 0 {
			b, _ := br.Peek(n)
			_, _ = up.Write(b)
		}
		_, _ = io.Copy(up, c)
		closeWrite(up.Conn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(c, up)
		closeWrite(c)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// hopHeaders are dropped from a forwarded request: they are the proxy's, or
// this hop's, and never the origin's. Upgrade goes, so no protocol switches.
var hopHeaders = []string{"Proxy-Authorization", "Proxy-Connection", "Connection", "Keep-Alive",
	"Proxy-Authenticate", "Te", "Trailer", "Upgrade"}

// forward sends a plain HTTP request to the checked address and copies the
// response back, then closes.
func (p *Proxy) forward(c net.Conn, req *http.Request, t Target, up *counted) {
	for _, f := range strings.Split(req.Header.Get("Connection"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			req.Header.Del(f)
		}
	}
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	req.URL = t.URL
	req.Host = t.URL.Host
	req.RequestURI = ""
	req.Close = true
	// The body is read from the client past the header limit.
	if err := req.Write(up); err != nil {
		writeStatus(c, http.StatusBadGateway, "abhed egress: sending the request failed\n", nil)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(up), req)
	if err != nil {
		writeStatus(c, http.StatusBadGateway, "abhed egress: the upstream response was malformed\n", nil)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	resp.Close = true
	_ = resp.Write(c)
}

// writeStatus answers with a short plain-text status and closes.
func writeStatus(c net.Conn, code int, body string, h http.Header) {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", code, http.StatusText(code))
	for k, vs := range h {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, b.String())
}

// counted counts the bytes through an upstream connection: out is what
// was sent to it, in what came back.
type counted struct {
	net.Conn
	in, out atomic.Int64
	act     *activity
}

func (c *counted) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.in.Add(int64(n))
	if n > 0 {
		c.act.touch()
	}
	return n, err
}

func (c *counted) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.out.Add(int64(n))
	if n > 0 {
		c.act.touch()
	}
	return n, err
}

// activity is when bytes last moved on a connection through the proxy,
// either way.
type activity struct{ last atomic.Int64 }

func (a *activity) touch() { a.last.Store(time.Now().UnixNano()) }

func (a *activity) since() time.Duration { return time.Since(time.Unix(0, a.last.Load())) }

// active is the client's connection, marking activity as bytes move.
type active struct {
	net.Conn
	act *activity
}

func (c *active) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.act.touch()
	}
	return n, err
}

func (c *active) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.act.touch()
	}
	return n, err
}

func (c *active) CloseWrite() error {
	closeWrite(c.Conn)
	return nil
}

// watchIdle closes conns once no bytes have moved for idle, until the
// returned function is called, which reports whether it did.
func watchIdle(idle time.Duration, act *activity, conns ...net.Conn) func() bool {
	tick := max(idle/4, 10*time.Millisecond)
	stop := make(chan struct{})
	done := make(chan struct{})
	var fired atomic.Bool
	go func() {
		defer close(done)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if act.since() >= idle {
					fired.Store(true)
					for _, c := range conns {
						_ = c.Close()
					}
					return
				}
			}
		}
	}()
	return func() bool {
		close(stop)
		<-done
		return fired.Load()
	}
}

// headError words a failure to read a request's head for the record,
// without what the client sent.
func headError(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, errHeadTooLarge):
		return fmt.Sprintf("the request head is over %d bytes", maxHeader)
	case errors.As(err, &ne) && ne.Timeout():
		return fmt.Sprintf("the request head did not arrive within %s", headerTimeout)
	}
	return "the request head is not HTTP/1.x"
}

// headerLimit bounds what a request's head may read, then is lifted for
// its body.
type headerLimit struct {
	r    io.Reader
	left int64
}

func (l *headerLimit) Read(b []byte) (int, error) {
	if l.left < 0 {
		return l.r.Read(b)
	}
	if l.left == 0 {
		return 0, errHeadTooLarge
	}
	if int64(len(b)) > l.left {
		b = b[:l.left]
	}
	n, err := l.r.Read(b)
	l.left -= int64(n)
	return n, err
}

func (l *headerLimit) lift() { l.left = -1 }

var errHeadTooLarge = errors.New("request head too large")
