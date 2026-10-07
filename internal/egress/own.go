package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Abhed's own requests (the model client, web_fetch, web_search, MCP over
// HTTP) are judged in-process by the same policy the commands' proxy uses.

// Kinds of Abhed's own requests, as an Event's Kind records them.
const (
	KindModel     = "model"
	KindWebFetch  = "web_fetch"
	KindWebSearch = "web_search"
	KindMCP       = "mcp"
)

// RuleModel is the rule that allows the model client's own endpoint.
const RuleModel = "model"

// Caller is who one of Abhed's own requests is made for.
type Caller struct {
	Session string
	CallID  string
	// Record writes one event to the session's record; nil outside a session.
	Record func(event string, payload map[string]any) error
}

type callerKey struct{}

// WithCaller names the session, and the call, that ctx's requests are made for.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerOf is the Caller ctx names, or the zero Caller.
func CallerOf(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}

// GuardOptions configure a Guard.
type GuardOptions struct {
	// Policy is the compiled egress section; nil with Err refuses every
	// request but the model's.
	Policy *Policy
	Err    error
	// Resolve looks a name up; nil uses the system resolver.
	Resolve     func(ctx context.Context, host string) ([]netip.Addr, error)
	DialTimeout time.Duration
	// Burst, AllowBudget and Interval are the record's limits, as for a Proxy.
	Burst       int
	AllowBudget int
	Interval    time.Duration
}

// Guard judges Abhed's own requests by the egress policy and records each
// decision in the record of the session it was made for.
type Guard struct {
	opts GuardOptions

	mu       sync.Mutex
	sessions map[string]*ownSession
	closed   bool
	done     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
}

// ownSession is one session's record limits and where its decisions go.
type ownSession struct {
	id   string
	lim  *limiter
	used bool // under Guard.mu; a session unused for an interval is let go

	mu     sync.Mutex
	routes map[string]func(string, map[string]any) error
	latest func(string, map[string]any) error
}

// NewGuard starts a guard; Close stops it.
func NewGuard(o GuardOptions) *Guard {
	if o.Policy == nil && o.Err == nil {
		o.Err = errors.New("no policy")
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.Interval <= 0 {
		o.Interval = defaultInterval
	}
	g := &Guard{opts: o, sessions: map[string]*ownSession{}, done: make(chan struct{})}
	g.wg.Add(1)
	go g.sweep()
	return g
}

// sweep lets go of sessions that made no request for a whole interval,
// recording what their limiters still count.
func (g *Guard) sweep() {
	defer g.wg.Done()
	t := time.NewTicker(g.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-g.done:
			return
		case <-t.C:
		}
		var stale []*ownSession
		g.mu.Lock()
		for id, s := range g.sessions {
			if !s.used {
				delete(g.sessions, id)
				stale = append(stale, s)
			}
			s.used = false
		}
		g.mu.Unlock()
		for _, s := range stale {
			s.lim.stop()
		}
	}
}

// EndSession records what the session id's limiter still counts and lets it go.
func (g *Guard) EndSession(id string) {
	g.mu.Lock()
	s := g.sessions[id]
	delete(g.sessions, id)
	g.mu.Unlock()
	if s != nil {
		s.lim.stop()
	}
}

// Close records what every session's limiter still counts; requests after
// it are still judged, and recorded one by one.
func (g *Guard) Close() {
	g.once.Do(func() {
		g.mu.Lock()
		g.closed = true
		all := g.sessions
		g.sessions = map[string]*ownSession{}
		g.mu.Unlock()
		close(g.done)
		g.wg.Wait()
		for _, s := range all {
			s.lim.stop()
		}
	})
}

// session is c's session's state, begun if need be, with c's record
// remembered for its call; nil once the guard is closed.
func (g *Guard) session(c Caller) *ownSession {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	s := g.sessions[c.Session]
	if s == nil {
		s = &ownSession{id: c.Session}
		s.lim = newLimiter(g.opts.Burst, g.opts.AllowBudget, g.opts.Interval, s.emit)
		g.sessions[c.Session] = s
	}
	s.used = true
	if c.Record != nil {
		s.mu.Lock()
		if s.routes == nil || len(s.routes) >= maxOwnRoutes {
			s.routes = map[string]func(string, map[string]any) error{}
		}
		s.routes[c.CallID] = c.Record
		s.latest = c.Record
		s.mu.Unlock()
	}
	return s
}

// maxOwnRoutes bounds the calls whose records a session's state remembers.
const maxOwnRoutes = 4096

// emit writes e to the record of the call it names, or the session's latest.
func (s *ownSession) emit(e Event) {
	s.mu.Lock()
	rec := s.routes[e.CallID]
	if rec == nil {
		rec = s.latest
	}
	s.mu.Unlock()
	writeOwn(s.id, rec, e)
}

// writeOwn writes e with its session; with no record, it is logged.
func writeOwn(session string, rec func(string, map[string]any) error, e Event) {
	m := e.Payload()
	if e.Kind != "http" && e.Method != "" {
		m["method"], m["path"] = e.Method, e.Path
	}
	if session != "" {
		m["session"] = session
	}
	if rec == nil {
		slog.Warn("egress decision of Abhed's own request, outside any session's record", "kind", e.Kind,
			"host", e.Host, "port", e.Port, "decision", string(e.Decision), "rule", e.Rule, "reason", e.Reason)
		return
	}
	_ = rec(EventName, m)
}

// record passes e to the limiter of c's session.
func (g *Guard) record(c Caller, e Event) {
	if g.opts.Policy != nil && !g.opts.Policy.RecordPaths() {
		e.Path = ""
	}
	if s := g.session(c); s != nil {
		s.lim.record(e)
		return
	}
	writeOwn(c.Session, c.Record, e)
}

// installed are the guards Install put in force, the latest last.
var installed struct {
	mu   sync.Mutex
	list []*Guard
}

// Install puts g in force for every Transport that names no guard, until
// the returned function is called.
func Install(g *Guard) (uninstall func()) {
	installed.mu.Lock()
	installed.list = append(installed.list, g)
	installed.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			installed.mu.Lock()
			defer installed.mu.Unlock()
			for i := len(installed.list) - 1; i >= 0; i-- {
				if installed.list[i] == g {
					installed.list = append(installed.list[:i], installed.list[i+1:]...)
					return
				}
			}
		})
	}
}

// Installed is the guard in force, or nil outside the allowlist.
func Installed() *Guard {
	installed.mu.Lock()
	defer installed.mu.Unlock()
	if n := len(installed.list); n > 0 {
		return installed.list[n-1]
	}
	return nil
}

// DeniedError is one of Abhed's own requests the egress policy refused.
type DeniedError struct {
	Host   string
	Port   uint16
	Rule   string
	Reason string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("abhed egress: %s refused by %s: %s",
		net.JoinHostPort(e.Host, strconv.Itoa(int(e.Port))), e.Rule, e.Reason)
}

// Transport is one of Abhed's own clients' round tripper: Base, unchanged,
// with no guard in force; with one, each request is judged and recorded.
type Transport struct {
	// Base is the client's own transport; nil is http.DefaultTransport.
	Base *http.Transport
	// Kind names the client in the record: one of the Kind constants.
	Kind string
	// Model, for the model client only, lists the endpoint URLs allowed
	// implicitly, read on each request since an adapter's may change.
	Model func() []string
	// Check is the client's own address check, applied after the policy's.
	Check func(host string, a netip.AddrPort) error
	// Guard is the guard to use; nil is the one installed.
	Guard *Guard

	mu      sync.Mutex
	guarded map[*Guard]*http.Transport
}

func (t *Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

// RoundTrip sends req, judged by the guard in force if there is one.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	g := t.Guard
	if g == nil {
		g = Installed()
	}
	if g == nil {
		return t.base().RoundTrip(req)
	}
	return g.roundTrip(t, req)
}

// CloseIdleConnections closes the idle connections of every pool t holds.
func (t *Transport) CloseIdleConnections() {
	if c, ok := t.base().(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.guarded {
		c.CloseIdleConnections()
	}
}

// forGuard is Base as g sends through it: no proxy from the environment, so
// the address check sees where the connection goes, and g's dialer.
func (t *Transport) forGuard(g *Guard) *http.Transport {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.guarded[g]; c != nil {
		return c
	}
	if t.guarded == nil {
		t.guarded = map[*Guard]*http.Transport{}
	}
	var c *http.Transport
	if b, ok := t.base().(*http.Transport); ok {
		c = b.Clone()
	} else {
		c = &http.Transport{ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: 90 * time.Second}
	}
	c.Proxy = nil
	c.DialContext = g.dial
	c.DialTLSContext = nil
	t.guarded[g] = c
	return c
}

// verdictKey carries a judged request's ownVerdict to the dialer.
type verdictKey struct{}

// ownVerdict is what one judged request may connect to.
type ownVerdict struct {
	host     string
	allowIPs []netip.Prefix
	anyAddr  bool // the model's endpoint: its own address, whatever it is
	check    func(string, netip.AddrPort) error

	mu      sync.Mutex
	ip      string
	refused *refusal
	failed  error // the client's own check's refusal
}

func (o *ownVerdict) refuse(r *refusal) {
	o.mu.Lock()
	if o.refused == nil {
		o.refused = r
	}
	o.mu.Unlock()
}

// addrOK checks one address against the verdict: a refusal from the policy,
// or the client's own check's error.
func (o *ownVerdict) addrOK(ap netip.AddrPort) error {
	if o.anyAddr {
		return nil
	}
	a := ap.Addr().Unmap()
	if !named(o.allowIPs, a) {
		if why := AddrRefusal(a); why != "" {
			r := &refusal{host: o.host, addr: a, why: why}
			o.refuse(r)
			return r
		}
	}
	if o.check != nil {
		if err := o.check(o.host, netip.AddrPortFrom(a, ap.Port())); err != nil {
			o.fail(err)
			return err
		}
	}
	return nil
}

func (o *ownVerdict) fail(err error) {
	o.mu.Lock()
	if o.failed == nil {
		o.failed = err
	}
	o.mu.Unlock()
}

// gotConn checks the address of the connection the request is given, which
// may have been dialled for another request with other allow_ips.
func (o *ownVerdict) gotConn(info httptrace.GotConnInfo) {
	ta, ok := info.Conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		_ = info.Conn.Close()
		o.fail(errors.New("abhed egress: the connection is not TCP"))
		return
	}
	ap := ta.AddrPort()
	o.mu.Lock()
	o.ip = ap.Addr().Unmap().String()
	o.mu.Unlock()
	if o.addrOK(ap) != nil {
		_ = info.Conn.Close()
	}
}

// dial resolves once, checks every address against the verdict of the
// request it is for, and connects to one it checked.
func (g *Guard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	ov, _ := ctx.Value(verdictKey{}).(*ownVerdict)
	if ov == nil {
		return nil, errors.New("abhed egress: a connection for no judged request is refused")
	}
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := parsePort(ps)
	if err != nil {
		return nil, err
	}
	addrs, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s has no address", host)
	}
	// A name with any refused address is refused whole, as in the proxy.
	for _, a := range addrs {
		if err := ov.addrOK(netip.AddrPortFrom(a.Unmap(), port)); err != nil {
			return nil, err
		}
	}
	d := net.Dialer{Timeout: g.opts.DialTimeout}
	var last error
	for _, a := range addrs {
		c, err := d.DialContext(ctx, network, netip.AddrPortFrom(a.Unmap(), port).String())
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

func (g *Guard) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{a}, nil
	}
	if g.opts.Resolve != nil {
		return g.opts.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// ownTarget is the host, port and decoded path of one of Abhed's own requests.
func ownTarget(u *url.URL) (string, uint16, string, error) {
	var port uint16
	switch strings.ToLower(u.Scheme) {
	case "http":
		port = 80
	case "https":
		port = 443
	default:
		return "", 0, "", fmt.Errorf("the %s: scheme is not reached", u.Scheme)
	}
	h, err := CanonicalHost(u.Hostname())
	if err != nil {
		return "", 0, "", err
	}
	if p := u.Port(); p != "" {
		if port, err = parsePort(p); err != nil {
			return "", 0, "", err
		}
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	return h, port, path, nil
}

// plainOwnPath refuses a path the proxy would refuse, so a path rule judges
// Abhed's own requests as it judges a command's.
func plainOwnPath(u *url.URL, path string) error {
	lower := strings.ToLower(u.EscapedPath())
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "%2e") || strings.Contains(lower, "%00") {
		return errors.New("the path has an encoded slash, backslash, dot or NUL")
	}
	return plainPath(path)
}

// isModel reports whether host and port are one of t's model endpoints.
func (t *Transport) isModel(host string, port uint16) bool {
	if t.Model == nil {
		return false
	}
	for _, raw := range t.Model() {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if h, p, _, err := ownTarget(u); err == nil && h == host && p == port {
			return true
		}
	}
	return false
}

// decide judges one request; the bool is true for the model's endpoint.
func (g *Guard) decide(t *Transport, u *url.URL, host string, port uint16, method, path string) (Verdict, bool) {
	if t.isModel(host, port) {
		return Verdict{Decision: Allow, Rule: RuleModel, Reason: "the model provider's endpoint, allowed implicitly"}, true
	}
	if g.opts.Policy == nil {
		return Verdict{Decision: Deny, Rule: "policy",
			Reason: "the egress policy could not be loaded (" + g.opts.Err.Error() + "), so only the model's endpoint is reached"}, false
	}
	if err := plainOwnPath(u, path); err != nil {
		return Verdict{Decision: Deny, Rule: "path", Reason: err.Error()}, false
	}
	return g.opts.Policy.Decide(Request{Host: host, Port: port, Method: method, Path: path}), false
}

// roundTrip judges req, records the decision in its caller's record, and
// sends it if allowed, through t's pool for g.
func (g *Guard) roundTrip(t *Transport, req *http.Request) (*http.Response, error) {
	c := CallerOf(req.Context())
	ev := Event{CallID: c.CallID, Kind: t.Kind, Method: req.Method}
	host, port, path, err := ownTarget(req.URL)
	if err != nil {
		ev.Host, ev.Decision, ev.Rule, ev.Reason = req.URL.Hostname(), Deny, "parse", err.Error()
		g.record(c, ev)
		return nil, &DeniedError{Host: ev.Host, Rule: ev.Rule, Reason: ev.Reason}
	}
	ev.Host, ev.Port, ev.Path = host, port, path
	v, model := g.decide(t, req.URL, host, port, req.Method, path)
	ev.Decision, ev.Rule, ev.Reason = v.Decision, v.Rule, v.Reason
	if !v.Allowed() {
		g.record(c, ev)
		return nil, &DeniedError{Host: host, Port: port, Rule: v.Rule, Reason: v.Reason}
	}
	if req.ContentLength > 0 {
		ev.BytesOut = req.ContentLength
	}
	ov := &ownVerdict{host: host, allowIPs: v.AllowIPs, anyAddr: model, check: t.Check}
	ctx := context.WithValue(req.Context(), verdictKey{}, ov)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: ov.gotConn})
	resp, err := t.forGuard(g).RoundTrip(req.WithContext(ctx))
	ov.mu.Lock()
	refused, failed := ov.refused, ov.failed
	ev.IP = ov.ip
	ov.mu.Unlock()
	if refused != nil || failed != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		ev.Decision = Deny
		if refused != nil {
			ev.Reason = refused.Error()
			g.record(c, ev)
			return nil, &DeniedError{Host: host, Port: port, Rule: v.Rule, Reason: refused.Error()}
		}
		// The client's own check refused it: its error is returned as it is.
		ev.Rule, ev.Reason = t.Kind, failed.Error()
		g.record(c, ev)
		if err == nil {
			err = failed
		}
		return nil, err
	}
	if err != nil {
		ev.Reason += "; the request failed: " + err.Error()
		g.record(c, ev)
		return nil, err
	}
	g.record(c, ev)
	return resp, nil
}
