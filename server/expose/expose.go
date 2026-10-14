// Package expose publishes a service inside a sandbox through the server:
// an authenticated proxy path that only the principal who exposed it, in
// its tenant, can use, until it expires, is revoked, or the sandbox's
// incarnation ends. It is off unless the operator turns it on, and every
// exposure is recorded. The bytes reach the sandbox through the driver's
// forward channel, so a sandbox with no network serves without one.
//
// The REST route that creates exposures belongs to the API; it calls
// Server.Expose and mounts the Server at Options.Prefix behind the auth
// middleware.
package expose

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/driver"
)

// Defaults.
const (
	DefaultPrefix = "/v1/exposed/"
	DefaultMaxTTL = time.Hour
	// MaxPerSandbox bounds the exposures one sandbox may hold at once.
	MaxPerSandbox = 16
)

// csp makes a proxied page an opaque origin: its scripts cannot read the
// server's cookies or call its API as the viewer.
const csp = "sandbox allow-scripts allow-forms allow-popups allow-downloads"

// Options configure the server. The zero value is off.
type Options struct {
	// Enabled turns exposing on; it is off by default.
	Enabled bool
	// MaxTTL is the longest an exposure lives, and the default; one hour
	// when zero.
	MaxTTL time.Duration
	// Prefix is where the Server is mounted; DefaultPrefix when empty.
	Prefix string
	// Record receives each exposure's opening and closing.
	Record driver.Recorder
	// Current, when set, is asked on every request for the sandbox's
	// generation and whether it is Ready; an exposure whose generation is
	// no longer current is closed.
	Current func(ctx context.Context, sandboxID string) (generation int64, ready bool, err error)
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Request asks for one exposure. Owner and Tenant are the authenticated
// caller's, never the client's word.
type Request struct {
	Driver     driver.Driver
	SandboxID  string
	Generation int64
	Port       int
	TTL        time.Duration
	Owner      string
	Tenant     string
}

// Exposure is one published service.
type Exposure struct {
	ID         string    `json:"id"`
	SandboxID  string    `json:"sandbox_id"`
	Generation int64     `json:"generation"`
	Port       int       `json:"port"`
	Path       string    `json:"path"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type exposure struct {
	Exposure
	owner, tenant string
	d             driver.Driver
	transport     *http.Transport
	proxy         *httputil.ReverseProxy
	ctx           context.Context
	cancel        context.CancelFunc
	timer         *time.Timer
}

// Server holds the live exposures and serves their proxy path.
type Server struct {
	opts Options

	mu   sync.Mutex
	byID map[string]*exposure
}

// New returns a server with opts.
func New(opts Options) *Server {
	if opts.MaxTTL <= 0 {
		opts.MaxTTL = DefaultMaxTTL
	}
	if opts.Prefix == "" {
		opts.Prefix = DefaultPrefix
	}
	if !strings.HasSuffix(opts.Prefix, "/") {
		opts.Prefix += "/"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Server{opts: opts, byID: map[string]*exposure{}}
}

// Prefix is the path the server is mounted at.
func (s *Server) Prefix() string { return s.opts.Prefix }

// Enabled reports whether exposing is on.
func (s *Server) Enabled() bool { return s.opts.Enabled }

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "exp_" + hex.EncodeToString(b[:])
}

// Expose publishes req's port. It is refused while exposing is off, for a
// driver without forward, and past the per-sandbox bound.
func (s *Server) Expose(ctx context.Context, req Request) (Exposure, error) {
	if !s.opts.Enabled {
		return Exposure{}, driver.Errorf(driver.ErrUnsupported, "exposing sandbox services is off; the server's operator turns it on")
	}
	if req.Driver == nil || !req.Driver.Info().Has(driver.CapForward) {
		return Exposure{}, driver.Errorf(driver.ErrUnsupported, "the sandbox's driver cannot forward to a port")
	}
	if req.SandboxID == "" || req.Owner == "" || auth.OwnsNothing(req.Owner) {
		return Exposure{}, driver.Errorf(driver.ErrInvalid, "an exposure needs a sandbox and an owner")
	}
	if req.Port < 1 || req.Port > 65535 {
		return Exposure{}, driver.Errorf(driver.ErrInvalid, "port %d is out of range", req.Port)
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = s.opts.MaxTTL
	}
	if ttl > s.opts.MaxTTL {
		return Exposure{}, driver.Errorf(driver.ErrInvalid, "ttl %s is longer than the %s allowed", ttl, s.opts.MaxTTL)
	}
	var ended <-chan struct{}
	if lt, ok := req.Driver.(driver.Lifetime); ok {
		var err error
		if ended, err = lt.Done(req.SandboxID); err != nil {
			return Exposure{}, err
		}
	}
	e := &exposure{owner: req.Owner, tenant: req.Tenant, d: req.Driver}
	e.ID, e.SandboxID, e.Generation, e.Port = newID(), req.SandboxID, req.Generation, req.Port
	e.Path = s.opts.Prefix + e.ID + "/"
	e.ExpiresAt = s.opts.Now().Add(ttl).UTC()
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			rwc, err := driver.Forward(ctx, e.d, e.SandboxID, e.Port)
			if err != nil {
				return nil, err
			}
			return conn{rwc}, nil
		},
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	e.proxy = s.proxy(e)

	s.mu.Lock()
	n := 0
	for _, o := range s.byID {
		if o.SandboxID == req.SandboxID {
			n++
		}
	}
	if n >= MaxPerSandbox {
		s.mu.Unlock()
		return Exposure{}, driver.Errorf(driver.ErrConflict, "sandbox %s already has %d exposures", req.SandboxID, n)
	}
	s.byID[e.ID] = e
	s.mu.Unlock()

	if err := s.record(e, "open", map[string]any{"expires_at": e.ExpiresAt}); err != nil {
		s.close(e.ID, "")
		return Exposure{}, driver.Errorf(driver.ErrUnavailable, "recording the exposure: %v", err)
	}
	e.timer = time.AfterFunc(ttl, func() { s.close(e.ID, "expired") })
	go func() {
		select {
		case <-ended:
			s.close(e.ID, "sandbox stopped")
		case <-e.ctx.Done():
		}
	}()
	return e.Exposure, nil
}

// Revoke closes one exposure; false when there is none.
func (s *Server) Revoke(id string) bool { return s.close(id, "revoked") }

// RevokeSandbox closes every exposure of a sandbox, as stopping or
// deleting it does, and returns how many there were.
func (s *Server) RevokeSandbox(sandboxID string) int {
	s.mu.Lock()
	var ids []string
	for id, e := range s.byID {
		if e.SandboxID == sandboxID {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.close(id, "sandbox closed")
	}
	return len(ids)
}

// List returns the live exposures of a sandbox.
func (s *Server) List(sandboxID string) []Exposure {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Exposure
	for _, e := range s.byID {
		if e.SandboxID == sandboxID {
			out = append(out, e.Exposure)
		}
	}
	return out
}

// Close ends every exposure.
func (s *Server) Close() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.close(id, "server closed")
	}
}

// close removes an exposure, ends its connections, and records why;
// an empty reason records nothing.
func (s *Server) close(id, reason string) bool {
	s.mu.Lock()
	e, ok := s.byID[id]
	delete(s.byID, id)
	s.mu.Unlock()
	if !ok {
		return false
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	e.cancel()
	e.transport.CloseIdleConnections()
	if reason != "" {
		_ = s.record(e, "close", map[string]any{"reason": reason})
	}
	return true
}

func (s *Server) record(e *exposure, action string, extra map[string]any) error {
	p := map[string]any{"sandbox_id": e.SandboxID, "generation": e.Generation, "port": e.Port,
		"exposure_id": e.ID, "action": action, "owner": e.owner}
	for k, v := range extra {
		p[k] = v
	}
	if s.opts.Record == nil {
		slog.Warn("exposure not recorded: no record", "sandbox_id", e.SandboxID, "action", action)
		return nil
	}
	return s.opts.Record(driver.EventExpose, p)
}

// lookup finds the exposure a request names, if the caller may use it.
// Every refusal looks the same, so nobody learns another's exposure exists.
func (s *Server) lookup(r *http.Request) (*exposure, string, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, s.opts.Prefix)
	if !ok || !s.opts.Enabled {
		return nil, "", false
	}
	id, sub, _ := strings.Cut(rest, "/")
	s.mu.Lock()
	e, ok := s.byID[id]
	s.mu.Unlock()
	if !ok {
		return nil, "", false
	}
	who, ok := auth.FromContext(r.Context())
	if !ok || who == nil {
		return nil, "", false
	}
	owner := who.Owner()
	if auth.OwnsNothing(owner) || owner != e.owner || who.Tenant != e.tenant {
		return nil, "", false
	}
	if !s.opts.Now().Before(e.ExpiresAt) {
		s.close(e.ID, "expired")
		return nil, "", false
	}
	if s.opts.Current != nil {
		gen, ready, err := s.opts.Current(r.Context(), e.SandboxID)
		if err != nil || !ready || gen != e.Generation {
			s.close(e.ID, "sandbox changed")
			return nil, "", false
		}
	}
	return e, "/" + sub, true
}

// ServeHTTP proxies a request under the prefix to its exposure.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.serve(w, r, nil) }

// Guard is the server with one more check on every request: allow, asked
// with the exposure's sandbox, must still let the caller use that sandbox.
// A refusal is the same 404 as an unknown exposure.
func (s *Server) Guard(allow func(r *http.Request, sandboxID string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, allow) })
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, allow func(*http.Request, string) bool) {
	e, sub, ok := s.lookup(r)
	if ok && allow != nil {
		ok = allow(r, e.SandboxID)
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such exposure")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(e.ctx, cancel)
	defer stop()
	r2 := r.Clone(ctx)
	r2.URL.Path, r2.URL.RawPath = sub, ""
	e.proxy.ServeHTTP(w, r2)
}

// proxy forwards to the sandbox without the viewer's credentials, and
// passes back nothing that could act on the server's origin.
func (s *Server) proxy(e *exposure) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "127.0.0.1"
			pr.Out.Host = "127.0.0.1"
			for _, h := range []string{"Cookie", "Authorization", "Proxy-Authorization", "X-Abhed-Token"} {
				pr.Out.Header.Del(h)
			}
		},
		Transport: e.transport,
		ModifyResponse: func(resp *http.Response) error {
			h := resp.Header
			h.Del("Set-Cookie")
			h.Del("Clear-Site-Data")
			h.Del("Service-Worker-Allowed")
			h.Add("Content-Security-Policy", csp)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadGateway, "unavailable", "the sandbox's service did not answer: "+driver.Code(err))
		},
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// conn is a forward stream as a net.Conn for the HTTP transport. Deadlines
// are not supported; the transport's own timeouts and the request's
// context bound it instead.
type conn struct{ io.ReadWriteCloser }

func (conn) LocalAddr() net.Addr              { return addr{} }
func (conn) RemoteAddr() net.Addr             { return addr{} }
func (conn) SetDeadline(time.Time) error      { return nil }
func (conn) SetReadDeadline(time.Time) error  { return nil }
func (conn) SetWriteDeadline(time.Time) error { return nil }

type addr struct{}

func (addr) Network() string { return "abhed-forward" }
func (addr) String() string  { return "sandbox" }
