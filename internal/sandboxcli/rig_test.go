package sandboxcli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/profiles"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/server/expose"
)

// Tokens the rig's server accepts. Each is distinctive, so a test can tell
// that none was ever printed.
const (
	aliceToken = "tok-alice-5f2c9e1b"
	adminToken = "tok-admin-a81d4473"
	freshToken = "tok-fresh-77c0be21"
)

type verifier struct {
	mu sync.Mutex
	ok map[string]*auth.Identity
}

func (v *verifier) Verify(_ context.Context, token string) (*auth.Identity, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if id, ok := v.ok[token]; ok {
		return id, nil
	}
	return nil, errors.New("unknown token")
}

type stubAdapter struct{}

func (stubAdapter) Name() string                           { return "stub" }
func (stubAdapter) Profile() model.Profile                 { return model.Profile{Name: "stub", ContextWindow: 32000} }
func (stubAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (stubAdapter) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk)
	close(ch)
	return ch, nil
}

// rig is a real server with the control plane on the fake driver, served
// over a real connection, and a profiles file signed in to it.
type rig struct {
	url        string
	svc        *controlplane.Service
	store      *controlplane.Memory
	fake       *drivertest.Fake
	hostKey    ssh.Signer
	profiles   string
	knownHosts string
	// requests counts what reached the server; wrap, when set, sees each
	// request first and may answer it itself.
	requests atomic.Int64
	mu       sync.Mutex
	wrap     func(w http.ResponseWriter, r *http.Request, next http.Handler) bool
	// sshArgs is what the last ssh would have run with; sshExit its status.
	sshArgs []string
	sshExit int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	fake := drivertest.New("process", driver.CapAttach, driver.CapCopy, driver.CapForward, driver.CapSnapshot)
	shellFake(fake)
	reg := driver.NewRegistry()
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	st, err := controlplane.NewSettings(map[string]string{"reconcile.interval_seconds": "1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mem := controlplane.NewMemory()
	ws := t.TempDir()
	svc, err := controlplane.New(controlplane.Config{Store: mem, Audit: mem, Drivers: reg, Settings: st,
		Workspace: func(string) (string, error) { return ws, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = svc.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.Mode = "oidc"
	cfg.Permissions.Deny = []string{"bash(rm *)"}
	v := &verifier{ok: map[string]*auth.Identity{
		aliceToken: {Subject: "alice", Tenant: "default"},
		freshToken: {Subject: "alice", Tenant: "default"},
		adminToken: {Subject: "root", Tenant: "default", Groups: []string{server.DefaultAdminGroup}},
	}}
	ex := expose.New(expose.Options{Enabled: true, Current: svc.Current})
	srv := server.New(server.Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Auth: &auth.Middleware{PublicPaths: server.PublicPaths(), Verifier: v},
		ControlPlane: svc, Expose: ex, SandboxSSHKey: key, StreamRecheck: 100 * time.Millisecond})
	r := &rig{svc: svc, store: mem, fake: fake, hostKey: key}
	h := srv.Handler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/test/token" {
			_ = req.ParseForm()
			if req.Form.Get("grant_type") != "refresh_token" || req.Form.Get("refresh_token") != "refresh-1" {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"`+freshToken+`","token_type":"Bearer","expires_in":3600}`)
			return
		}
		r.requests.Add(1)
		r.mu.Lock()
		wrap := r.wrap
		r.mu.Unlock()
		if wrap != nil && wrap(w, req, h) {
			return
		}
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(hs.Close)
	r.url = hs.URL
	dir := t.TempDir()
	r.profiles = filepath.Join(dir, "profiles.json")
	r.knownHosts = filepath.Join(dir, "sandbox_known_hosts")
	f := &profiles.File{Version: 1, Current: "alice", Profiles: map[string]*profiles.Profile{
		"alice": {URL: hs.URL, Token: aliceToken, TokenType: "Bearer"},
		"admin": {URL: "http://127.0.0.1:1", Token: adminToken},
	}}
	f.Profiles["admin"].URL = hs.URL
	if err := profiles.Save(r.profiles, f); err != nil {
		t.Fatal(err)
	}
	return r
}

// setWrap sets what sees each request before the server.
func (r *rig) setWrap(f func(w http.ResponseWriter, req *http.Request, next http.Handler) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wrap = f
}

// shellFake makes the fake's shell echo its keys, exit with 7 on "exit",
// and note its size.
func shellFake(f *drivertest.Fake) {
	f.AttachFunc = func(ctx context.Context, _ string, req driver.AttachRequest) (driver.ExecResult, error) {
		lines := make(chan string)
		go func() {
			defer close(lines)
			buf := make([]byte, 4096)
			for {
				n, err := req.Stdin.Read(buf)
				if n > 0 {
					_, _ = req.Stdout.Write(buf[:n])
					lines <- string(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}()
		_, _ = req.Stdout.Write([]byte("$ "))
		for {
			select {
			case <-ctx.Done():
				return driver.ExecResult{}, ctx.Err()
			case ws := <-req.Resize:
				_, _ = req.Stdout.Write([]byte("size " + strconv.Itoa(int(ws.Rows)) + "x" + strconv.Itoa(int(ws.Cols)) + "\r\n$ "))
			case l, ok := <-lines:
				if !ok {
					return driver.ExecResult{ExitCode: 0}, nil
				}
				if strings.Contains(l, "exit") {
					return driver.ExecResult{ExitCode: 7}, nil
				}
			}
		}
	}
}

// syncBuffer is an output both the command and the test use at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// wait waits for the buffer to hold want.
func (s *syncBuffer) wait(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(s.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %q; have %q", want, s.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// result is one run of the command.
type result struct {
	code     int
	out, err string
}

// runOpts changes how one run is made.
type runOpts struct {
	ctx     context.Context
	in      io.Reader
	term    Terminal
	inAgent string
	out     *syncBuffer
	errOut  *syncBuffer
	listen  func(net.Addr)
}

func (r *rig) env(o runOpts) Env {
	if o.ctx == nil {
		o.ctx = context.Background()
	}
	if o.in == nil {
		o.in = strings.NewReader("")
	}
	if o.out == nil {
		o.out = &syncBuffer{}
	}
	if o.errOut == nil {
		o.errOut = &syncBuffer{}
	}
	return Env{Ctx: o.ctx, ProfilesPath: r.profiles, KnownHosts: r.knownHosts, In: o.in, Out: o.out, Err: o.errOut,
		Term: o.term, InAgent: func() string { return o.inAgent }, Executable: "/opt/abhed tools/abhed",
		Listening: o.listen,
		SSH: func(_ context.Context, args []string) (int, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.sshArgs = args
			return r.sshExit, nil
		}}
}

func (r *rig) runWith(t *testing.T, o runOpts, args ...string) result {
	t.Helper()
	if o.out == nil {
		o.out = &syncBuffer{}
	}
	if o.errOut == nil {
		o.errOut = &syncBuffer{}
	}
	code := Main(r.env(o), args)
	res := result{code: code, out: o.out.String(), err: o.errOut.String()}
	for _, tok := range []string{aliceToken, adminToken, freshToken, "refresh-1"} {
		if strings.Contains(res.out, tok) || strings.Contains(res.err, tok) {
			t.Fatalf("%v printed a token: %+v", args, res)
		}
	}
	return res
}

func (r *rig) run(t *testing.T, args ...string) result {
	t.Helper()
	return r.runWith(t, runOpts{}, args...)
}

// must runs a command that has to succeed.
func (r *rig) must(t *testing.T, args ...string) result {
	t.Helper()
	res := r.run(t, args...)
	if res.code != 0 {
		t.Fatalf("%v: exit %d\nout: %s\nerr: %s", args, res.code, res.out, res.err)
	}
	return res
}

// fakeTerm is a terminal whose size the test changes.
type fakeTerm struct {
	mu         sync.Mutex
	rows, cols uint16
	raw        int
	resized    chan struct{}
}

func (f *fakeTerm) MakeRaw() (func(), error) {
	f.mu.Lock()
	f.raw++
	f.mu.Unlock()
	return func() { f.mu.Lock(); f.raw--; f.mu.Unlock() }, nil
}

func (f *fakeTerm) Size() (uint16, uint16, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, f.cols, true
}

func (f *fakeTerm) Resized() (<-chan struct{}, func()) { return f.resized, func() {} }

func (f *fakeTerm) resize(rows, cols uint16) {
	f.mu.Lock()
	f.rows, f.cols = rows, cols
	f.mu.Unlock()
	f.resized <- struct{}{}
}

// pipeConn joins two pipes into a net.Conn, for an SSH client whose
// transport is the ProxyCommand's stdin and stdout.
type pipeConn struct {
	io.Reader
	io.WriteCloser
}

func (pipeConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (pipeConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (pipeConn) SetDeadline(time.Time) error      { return nil }
func (pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (pipeConn) SetWriteDeadline(time.Time) error { return nil }
