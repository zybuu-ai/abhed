package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Upstream is a corporate proxy every one of Abhed's connections goes out
// through: its own clients' and, chained after the allowlist has decided,
// the commands'. It is an explicit HTTP proxy, reached over http or https;
// a PAC file is not read. Its credentials are read from the secrets store
// at each connection and only ever sent to it, in a Proxy-Authorization
// header: never in a URL, an error, a record or an environment.
type Upstream struct {
	// URL is the proxy: http://host:port or https://host:port, no user, path or query.
	URL *url.URL
	// Roots verify the proxy's certificate and, as a proxy that inspects
	// TLS re-signs them, the servers reached through it: the system's and
	// Bundle's; nil is the system's alone.
	Roots *x509.CertPool
	// Bundle are the certificates of the operator's CA bundle, added too to
	// a client's own roots (a cluster's CA, say).
	Bundle []*x509.Certificate
	// Credentials returns the user and password, or ok false for none.
	Credentials func() (user, password string, ok bool, err error)
	// NoProxy are the destinations reached directly: host names (a leading
	// dot or *. matches the domain and its subdomains), IP addresses and
	// CIDR ranges. Loopback is always reached directly.
	NoProxy []string
	// ResolveViaProxy lets a name that does not resolve here at all go to
	// the proxy unchecked, for a host a rule names exactly; off, it is refused.
	ResolveViaProxy bool
}

// ErrUpstreamCredentials is a proxy credential that could not be read.
var ErrUpstreamCredentials = errors.New("abhed egress: the upstream proxy's credentials could not be read")

var upstream atomic.Pointer[Upstream]

// SetUpstream puts u, or none for nil, in force for every connection made
// from now on.
func SetUpstream(u *Upstream) { upstream.Store(u) }

// upstreamUsed is set once UseUpstream has chosen the process's proxy, or none.
var upstreamUsed struct {
	sync.Mutex
	done bool
	key  string
}

// ErrUpstreamConflict is a second, different upstream proxy asked for in one
// process: the proxy is process-wide, so it is refused rather than either
// configuration running through the other's proxy, or none.
var ErrUpstreamConflict = errors.New("abhed egress: this process already goes out through a different upstream proxy (or none); network.upstream_proxy is process-wide, so a session with another one runs in a process of its own")

// UseUpstream puts u (nil for none) in force for the process the first time
// it is called; later, the same proxy again is accepted and any other refused.
// key describes the configuration, so two equal ones are the same proxy.
func UseUpstream(u *Upstream, key string) error {
	upstreamUsed.Lock()
	defer upstreamUsed.Unlock()
	if upstreamUsed.done {
		if key != upstreamUsed.key {
			return ErrUpstreamConflict
		}
		return nil
	}
	upstreamUsed.done, upstreamUsed.key = true, key
	SetUpstream(u)
	return nil
}

// ResetUpstreamForTest forgets the process's choice; tests only.
func ResetUpstreamForTest() {
	upstreamUsed.Lock()
	upstreamUsed.done, upstreamUsed.key = false, ""
	upstreamUsed.Unlock()
	SetUpstream(nil)
}

// CurrentUpstream is the upstream proxy in force, or nil.
func CurrentUpstream() *Upstream { return upstream.Load() }

// Authority is the proxy's host:port, as records name it.
func (u *Upstream) Authority() string {
	if u.URL.Port() != "" {
		return u.URL.Host
	}
	if u.URL.Scheme == "https" {
		return net.JoinHostPort(u.URL.Hostname(), "443")
	}
	return net.JoinHostPort(u.URL.Hostname(), "80")
}

// Bypass reports whether host is reached directly: loopback, or in NoProxy.
func (u *Upstream) Bypass(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	a, perr := netip.ParseAddr(h)
	isIP := perr == nil
	if isIP && a.Unmap().IsLoopback() {
		return true
	}
	for _, e := range u.NoProxy {
		e = strings.ToLower(strings.TrimSpace(e))
		switch {
		case e == "":
		case isIP && strings.Contains(e, "/"):
			if p, err := netip.ParsePrefix(e); err == nil && p.Contains(a.Unmap()) {
				return true
			}
		case isIP:
			if b, err := netip.ParseAddr(e); err == nil && b.Unmap() == a.Unmap() {
				return true
			}
		case strings.HasPrefix(e, "*."):
			if d := e[1:]; h == d[1:] || strings.HasSuffix(h, d) {
				return true
			}
		case strings.HasPrefix(e, "."):
			if h == e[1:] || strings.HasSuffix(h, e) {
				return true
			}
		case h == e:
			return true
		}
	}
	return false
}

// authorization is the Proxy-Authorization value, or "" with no credentials.
func (u *Upstream) authorization() (string, error) {
	if u.Credentials == nil {
		return "", nil
	}
	user, pass, ok, err := u.Credentials()
	if err != nil {
		// The store's error may name the secret, never its value.
		return "", fmt.Errorf("%w: %w", ErrUpstreamCredentials, err)
	}
	if !ok {
		return "", nil
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)), nil
}

// tlsConfig is base with the upstream's roots added, so a server re-signed
// by an inspecting proxy, and the proxy itself, verify.
func (u *Upstream) tlsConfig(base *tls.Config) *tls.Config {
	var c *tls.Config
	if base != nil {
		c = base.Clone()
	} else {
		c = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	switch {
	case c.RootCAs == nil:
		c.RootCAs = u.Roots
	case len(u.Bundle) > 0:
		pool := c.RootCAs.Clone()
		for _, cert := range u.Bundle {
			pool.AddCert(cert)
		}
		c.RootCAs = pool
	}
	return c
}

// Apply makes t send through u whatever u does not bypass: CONNECT for
// https, absolute-form for http, each with u's credentials. The returned
// round tripper adds them to plain http requests, which CONNECT does not carry.
func (u *Upstream) Apply(t *http.Transport) http.RoundTripper {
	// A destination reached directly gets the transport as it was, with no
	// proxy from the environment and none of the bundle's trust.
	direct := t.Clone()
	direct.Proxy = nil
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		if u.Bypass(req.URL.Hostname()) {
			return nil, nil
		}
		return u.URL, nil
	}
	t.GetProxyConnectHeader = func(context.Context, *url.URL, string) (http.Header, error) {
		auth, err := u.authorization()
		if err != nil || auth == "" {
			return nil, err
		}
		return http.Header{"Proxy-Authorization": {auth}}, nil
	}
	t.TLSClientConfig = u.tlsConfig(t.TLSClientConfig)
	return &upstreamAuth{u: u, base: t, direct: direct}
}

// upstreamAuth adds the proxy's credentials to a plain http request it sends
// through the proxy, and to nothing else.
type upstreamAuth struct {
	u      *Upstream
	base   *http.Transport // through the proxy
	direct *http.Transport // for what u bypasses
}

func (a *upstreamAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	if a.u.Bypass(req.URL.Hostname()) {
		return a.direct.RoundTrip(req)
	}
	if !strings.EqualFold(req.URL.Scheme, "http") {
		return a.base.RoundTrip(req)
	}
	auth, err := a.u.authorization()
	if err != nil {
		closeBody(req)
		return nil, err
	}
	if auth == "" {
		return a.base.RoundTrip(req)
	}
	r := req.Clone(req.Context())
	r.Header.Set("Proxy-Authorization", auth)
	return a.base.RoundTrip(r)
}

func (a *upstreamAuth) CloseIdleConnections() {
	a.base.CloseIdleConnections()
	a.direct.CloseIdleConnections()
}

// dialProxy connects to the proxy itself, over TLS for an https proxy.
func (u *Upstream) dialProxy(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", u.Authority())
	if err != nil {
		return nil, fmt.Errorf("the upstream proxy %s: %w", u.Authority(), err)
	}
	if u.URL.Scheme != "https" {
		return c, nil
	}
	tc := tls.Client(c, u.tlsConfig(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.URL.Hostname()}))
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("the upstream proxy %s: %w", u.Authority(), err)
	}
	return tc, nil
}

// DialTunnel opens a tunnel to authority (host:port) through the proxy with
// CONNECT. The proxy's refusal is an error naming its status, never a header.
func (u *Upstream) DialTunnel(ctx context.Context, authority string, timeout time.Duration) (net.Conn, error) {
	if authority == "" || strings.ContainsAny(authority, "\r\n \t") {
		return nil, fmt.Errorf("abhed egress: %q is not a host and port to tunnel to", authority)
	}
	auth, err := u.authorization()
	if err != nil {
		return nil, err
	}
	c, err := u.dialProxy(ctx, timeout)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	req := "CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n"
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	if _, err := c.Write([]byte(req + "\r\n")); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("the upstream proxy %s: %w", u.Authority(), err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("the upstream proxy %s did not answer the CONNECT: %w", u.Authority(), err)
	}
	_ = resp.Body.Close() // a CONNECT answer's body is not the tunnel's
	if resp.StatusCode != http.StatusOK {
		_ = c.Close()
		return nil, &UpstreamRefused{Proxy: u.Authority(), Target: authority, Status: resp.StatusCode}
	}
	_ = c.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		// Bytes the proxy sent past its answer belong to the tunnel.
		return &bufferedConn{Conn: c, r: br}, nil
	}
	return c, nil
}

// DialForward connects to the proxy for a plain http request, which is then
// written in absolute form with ForwardHeader's credentials.
func (u *Upstream) DialForward(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	return u.dialProxy(ctx, timeout)
}

// ForwardHeader is the Proxy-Authorization a plain http request through the
// proxy carries, or "".
func (u *Upstream) ForwardHeader() (string, error) { return u.authorization() }

// UpstreamRefused is the upstream proxy's answer other than 200 to a CONNECT.
type UpstreamRefused struct {
	Proxy, Target string
	Status        int
}

func (e *UpstreamRefused) Error() string {
	return fmt.Sprintf("the upstream proxy %s refused %s: %d %s", e.Proxy, e.Target, e.Status, http.StatusText(e.Status))
}

// bufferedConn reads what a reader already holds before the connection.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bufferedConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// precheck resolves host here for the internal-address check before a
// request is chained. A name that does not resolve here at all is passed to
// the proxy only with ResolveViaProxy and an anchor (a rule or list naming the
// host exactly), and viaProxy says so; any other failure to resolve refuses.
func (u *Upstream) precheck(ctx context.Context, resolve func(context.Context, string) ([]netip.Addr, error), host string, anchored bool) (addrs []netip.Addr, viaProxy bool, err error) {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{a}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err = resolve(ctx, host)
	if err == nil {
		if len(addrs) == 0 {
			return nil, false, &refusal{host: host, msg: host + " has no address here, so it is not sent to the upstream proxy"}
		}
		return addrs, false, nil
	}
	var dnsErr *net.DNSError
	switch {
	case !errors.As(err, &dnsErr) || !dnsErr.IsNotFound:
		return nil, false, &refusal{host: host, msg: fmt.Sprintf("%s could not be resolved here (%s), so its address cannot be checked; it is not sent to the upstream proxy", host, resolveWhy(err))}
	case !u.ResolveViaProxy:
		return nil, false, &refusal{host: host, msg: host + " does not resolve here, so its address cannot be checked; it is not sent to the upstream proxy unless network.upstream_proxy.resolve_via_proxy is on"}
	case !anchored:
		return nil, false, &refusal{host: host, msg: host + " does not resolve here; resolve_via_proxy passes such a name only for a host an allow rule (or, for web_fetch outside the allowlist, the managed allowed_hosts) names exactly, not by a wildcard or the default"}
	}
	return nil, true, nil
}

// resolveWhy is a resolver failure in a few words, for a refusal.
func resolveWhy(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &dnsErr) && dnsErr.IsTimeout:
		return "the lookup timed out"
	case errors.As(err, &dnsErr) && dnsErr.IsTemporary:
		return "the resolver failed for now"
	}
	return "the lookup failed"
}

// clientCache holds each base transport's copy with an upstream applied.
type clientCache struct {
	mu sync.Mutex
	m  map[clientKey]http.RoundTripper
}

type clientKey struct {
	base *http.Transport
	up   *Upstream
}

var upstreamClients = &clientCache{m: map[clientKey]http.RoundTripper{}}

// maxUpstreamClients bounds the cache: one per client and upstream.
const maxUpstreamClients = 256

// UpstreamTransport is base, or http.DefaultTransport for nil, sending
// through the upstream proxy in force; with none, base as it is.
func UpstreamTransport(base *http.Transport) http.RoundTripper {
	u := CurrentUpstream()
	if base == nil {
		dt, _ := http.DefaultTransport.(*http.Transport)
		base = dt
	}
	if u == nil {
		if base == nil {
			return http.DefaultTransport
		}
		return base
	}
	k := clientKey{base: base, up: u}
	upstreamClients.mu.Lock()
	defer upstreamClients.mu.Unlock()
	if rt := upstreamClients.m[k]; rt != nil {
		return rt
	}
	var t *http.Transport
	if base != nil {
		t = base.Clone()
	} else {
		t = &http.Transport{ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: 90 * time.Second}
	}
	// The proxy is reached as the operator named it, not through a client's
	// own dialer, whose address check is for the servers it reaches.
	if own := t.DialContext; own != nil {
		authority := u.Authority()
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.EqualFold(addr, authority) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			}
			return own(ctx, network, addr)
		}
	}
	rt := u.Apply(t)
	if len(upstreamClients.m) < maxUpstreamClients {
		upstreamClients.m[k] = rt
	}
	return rt
}

// UpstreamRoundTripper sends each request through UpstreamTransport(base),
// reading the upstream in force at each request.
type UpstreamRoundTripper struct{ Base *http.Transport }

func (r UpstreamRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return UpstreamTransport(r.Base).RoundTrip(req)
}

// UpstreamClient is an http.Client with timeout that sends through the
// upstream proxy in force, for Abhed's own clients the allowlist does not judge.
func UpstreamClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: UpstreamRoundTripper{}}
}

// ApplyUpstream makes t, a transport made for one use, send through the
// upstream proxy in force, or leaves it as it is with none.
func ApplyUpstream(t *http.Transport) http.RoundTripper {
	if u := CurrentUpstream(); u != nil {
		return u.Apply(t)
	}
	return t
}

// DialUpstream opens a TCP connection to addr (host:port): by a CONNECT
// tunnel through the upstream proxy in force unless it bypasses the host,
// else directly. For Abhed's own raw streams, such as a websocket.
func DialUpstream(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if u := CurrentUpstream(); u != nil && !u.Bypass(host) {
		return u.DialTunnel(ctx, addr, 30*time.Second)
	}
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}
