package expose

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/driver"
)

// tcpDriver forwards to a host port standing in for the sandbox's own.
type tcpDriver struct {
	target string
	caps   []driver.Capability
	done   chan struct{}
}

func (d *tcpDriver) Info() driver.Info {
	return driver.Info{Name: "fake", Version: "1", Capabilities: d.caps}
}
func (d *tcpDriver) Create(context.Context, driver.Spec) (driver.Status, error) {
	return driver.Status{}, nil
}
func (d *tcpDriver) Start(context.Context, string) (driver.Status, error) {
	return driver.Status{}, nil
}
func (d *tcpDriver) Stop(context.Context, string, time.Duration) (driver.Status, error) {
	return driver.Status{}, nil
}
func (d *tcpDriver) Delete(context.Context, string) error { return nil }
func (d *tcpDriver) Status(context.Context, string) (driver.Status, error) {
	return driver.Status{}, nil
}
func (d *tcpDriver) Exec(context.Context, string, driver.ExecRequest) (driver.ExecResult, error) {
	return driver.ExecResult{}, nil
}
func (d *tcpDriver) Forward(ctx context.Context, _ string, _ int) (io.ReadWriteCloser, error) {
	var dl net.Dialer
	return dl.DialContext(ctx, "tcp", d.target)
}
func (d *tcpDriver) Done(string) (<-chan struct{}, error) { return d.done, nil }

type recorder struct {
	mu   sync.Mutex
	list []map[string]any
}

func (r *recorder) record(_ string, p map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, p)
	return nil
}

func (r *recorder) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, p := range r.list {
		s := p["action"].(string)
		if reason, ok := p["reason"].(string); ok {
			s += ":" + reason
		}
		out = append(out, s)
	}
	return out
}

// upstream is the sandbox's service; it reports what reached it and tries
// to set a cookie on the server's origin.
func upstream(t *testing.T) (*tcpDriver, *http.Header) {
	t.Helper()
	seen := &http.Header{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*seen = r.Header.Clone()
		seen.Set("X-Path", r.URL.Path)
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "abhed_session", Value: "planted", Path: "/"})
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<script>fetch('/v1/sessions')</script>hello from "+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return &tcpDriver{target: srv.Listener.Addr().String(), caps: []driver.Capability{driver.CapForward}, done: make(chan struct{})}, seen
}

func as(r *http.Request, sub, tenant string) *http.Request {
	return r.WithContext(auth.WithIdentity(r.Context(), &auth.Identity{Subject: sub, Provider: auth.ProviderLocal, Tenant: tenant}))
}

func get(t *testing.T, s *Server, path, sub, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Cookie", "abhed_session=secret")
	r.Header.Set("Authorization", "Bearer secret-token")
	if sub != "" {
		r = as(r, sub, tenant)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func expose(t *testing.T, s *Server, d driver.Driver) Exposure {
	t.Helper()
	e, err := s.Expose(context.Background(), Request{Driver: d, SandboxID: "sbx_1", Generation: 1, Port: 8000,
		Owner: auth.LocalOwner("alice"), Tenant: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestOffByDefault(t *testing.T) {
	d, _ := upstream(t)
	s := New(Options{})
	if _, err := s.Expose(context.Background(), Request{Driver: d, SandboxID: "sbx_1", Port: 80, Owner: "local:alice"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("exposed while off: %v", err)
	}
	if w := get(t, s, DefaultPrefix+"exp_x/", "alice", "t1"); w.Code != http.StatusNotFound {
		t.Fatal(w.Code)
	}
}

// Only the principal who exposed it, in its tenant, reaches the service;
// the viewer's credentials never reach the sandbox, and nothing the
// sandbox answers can set a cookie on, or script, the server's origin.
func TestProxyIsOwnerScopedAndStripsCredentials(t *testing.T) {
	d, seen := upstream(t)
	rec := &recorder{}
	s := New(Options{Enabled: true, Record: rec.record})
	defer s.Close()
	e := expose(t, s, d)
	if !strings.HasPrefix(e.Path, DefaultPrefix+"exp_") || !strings.HasSuffix(e.Path, "/") {
		t.Fatalf("path %q", e.Path)
	}

	w := get(t, s, e.Path+"app/index.html", "alice", "t1")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello from /app/index.html") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if seen.Get("Cookie") != "" || seen.Get("Authorization") != "" {
		t.Fatalf("credentials reached the sandbox: %v", *seen)
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatal("the sandbox set a cookie on the server's origin")
	}
	if csp := w.Header().Values("Content-Security-Policy"); len(csp) == 0 || !strings.HasPrefix(csp[len(csp)-1], "sandbox") {
		t.Fatalf("no sandbox CSP: %v", csp)
	}

	for _, who := range []struct{ sub, tenant string }{{"bob", "t1"}, {"alice", "t2"}, {"", ""}} {
		if w := get(t, s, e.Path, who.sub, who.tenant); w.Code != http.StatusNotFound {
			t.Errorf("%s in %s reached it: %d", who.sub, who.tenant, w.Code)
		}
	}
	if acts := rec.actions(); len(acts) != 1 || acts[0] != "open" {
		t.Fatalf("records %v", acts)
	}
}

// An exposure ends when it expires, when the sandbox's incarnation ends,
// when its generation is no longer current, and when revoked; each is
// recorded.
func TestExposureEnds(t *testing.T) {
	d, _ := upstream(t)
	rec := &recorder{}
	now := time.Now()
	clock := func() time.Time { return now }
	gen := int64(1)
	s := New(Options{Enabled: true, Record: rec.record, Now: clock,
		Current: func(context.Context, string) (int64, bool, error) { return gen, true, nil }})
	defer s.Close()

	e := expose(t, s, d)
	now = now.Add(2 * time.Hour)
	if w := get(t, s, e.Path, "alice", "t1"); w.Code != http.StatusNotFound {
		t.Fatalf("expired: %d", w.Code)
	}
	now = time.Now()

	e = expose(t, s, d)
	gen = 2
	if w := get(t, s, e.Path, "alice", "t1"); w.Code != http.StatusNotFound {
		t.Fatalf("a stale generation: %d", w.Code)
	}
	gen = 1

	e = expose(t, s, d)
	close(d.done)
	deadline := time.Now().Add(5 * time.Second)
	for len(s.List("sbx_1")) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w := get(t, s, e.Path, "alice", "t1"); w.Code != http.StatusNotFound {
		t.Fatalf("after the sandbox stopped: %d", w.Code)
	}

	d.done = make(chan struct{})
	e = expose(t, s, d)
	if !s.Revoke(e.ID) || s.Revoke(e.ID) {
		t.Fatal("revoke")
	}
	want := []string{"open", "close:expired", "open", "close:sandbox changed", "open", "close:sandbox stopped", "open", "close:revoked"}
	if got := rec.actions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("records %v, want %v", got, want)
	}
}

func TestExposeRefusals(t *testing.T) {
	d, _ := upstream(t)
	s := New(Options{Enabled: true, MaxTTL: time.Minute})
	defer s.Close()
	base := Request{Driver: d, SandboxID: "sbx_1", Port: 8000, Owner: "local:alice"}
	cases := map[string]func(*Request){
		"ttl too long":  func(r *Request) { r.TTL = time.Hour },
		"port":          func(r *Request) { r.Port = 70000 },
		"no owner":      func(r *Request) { r.Owner = "" },
		"owner nobody":  func(r *Request) { r.Owner = auth.NobodyPrefix + "local" },
		"no forward":    func(r *Request) { r.Driver = &tcpDriver{} },
		"no sandbox id": func(r *Request) { r.SandboxID = "" },
	}
	for name, mut := range cases {
		r := base
		mut(&r)
		if _, err := s.Expose(context.Background(), r); err == nil {
			t.Errorf("%s: exposed", name)
		}
	}
	for i := 0; i < MaxPerSandbox; i++ {
		if _, err := s.Expose(context.Background(), base); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Expose(context.Background(), base); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("past the bound: %v", err)
	}
	if n := s.RevokeSandbox("sbx_1"); n != MaxPerSandbox {
		t.Fatalf("revoked %d", n)
	}
}

// The handler mounts behind the auth middleware like any route.
func TestMountedBehindMiddleware(t *testing.T) {
	d, _ := upstream(t)
	s := New(Options{Enabled: true})
	defer s.Close()
	e, err := s.Expose(context.Background(), Request{Driver: d, SandboxID: "sbx_1", Port: 8000, Owner: auth.Anonymous, Tenant: "default"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(DefaultPrefix, s)
	srv := httptest.NewServer(auth.Middleware{}.Wrap(mux))
	defer srv.Close()
	resp, err := http.Get(srv.URL + e.Path + "x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "hello from /x") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}
