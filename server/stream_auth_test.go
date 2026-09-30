package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// flipProvider recognises its cookie while allow is set, standing in for any
// sign-in layer whose answer changes mid-stream: a sign-out, a removed
// account, an access gate that now refuses.
type flipProvider struct{ allow atomic.Bool }

func (p *flipProvider) Name() string { return "flip" }
func (p *flipProvider) Identify(r *http.Request) (*auth.Identity, bool) {
	if c, err := r.Cookie("flip"); err != nil || c.Value != "bob" || !p.allow.Load() {
		return nil, false
	}
	return &auth.Identity{Subject: "bob", Tenant: "default"}, true
}
func (p *flipProvider) Routes(*http.ServeMux)                          {}
func (p *flipProvider) PublicPaths() []string                          { return nil }
func (p *flipProvider) SignIn() (string, string)                       { return "", "" }
func (p *flipProvider) SignOut(w http.ResponseWriter, _ *http.Request) {}

const streamTestEvery = 150 * time.Millisecond

// streamRig is a server whose only sign-in is flipProvider, with a running
// session "s1" owned by bob.
type streamRig struct {
	s    *Server
	srv  *httptest.Server
	prov *flipProvider
	live *liveSession
	seq  atomic.Int64
}

func newStreamRig(t *testing.T, mw func(*flipProvider) *auth.Middleware) *streamRig {
	t.Helper()
	prov := &flipProvider{}
	prov.allow.Store(true)
	cfg := config.Default()
	opts := Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), StreamRecheck: streamTestEvery}
	if mw != nil {
		opts.Auth = mw(prov)
	}
	s := New(opts)
	user, tenant := s.callerOf(context.Background(), &auth.Identity{Subject: "bob", Tenant: "default"})
	if mw == nil {
		user, tenant = s.callerOf(context.Background(), &auth.Identity{Subject: "anonymous", Tenant: "default"})
	}
	live := &liveSession{ID: "s1", User: user, Tenant: tenant, State: "running",
		ptys: map[string]*ptyRun{}}
	s.running["s1"] = live
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &streamRig{s: s, srv: srv, prov: prov, live: live}
}

func flipAuth(p *flipProvider) *auth.Middleware {
	return &auth.Middleware{Providers: []auth.Provider{p}, PublicPaths: PublicPaths()}
}

// emit appends the next event of s1 and returns its seq.
func (g *streamRig) emit(t *testing.T) int64 {
	t.Helper()
	seq := g.seq.Add(1)
	if err := g.s.store.Append(agent.Event{ID: fmt.Sprint("e", seq), SessionID: "s1", Seq: seq,
		Type: agent.EvAgentMessage, Payload: json.RawMessage(`{}`), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return seq
}

// sseLine is one field line of an SSE stream; closed marks its end.
type sseLine struct {
	text   string
	closed bool
}

// open starts a GET on path with bob's cookie and returns its lines.
func (g *streamRig) open(t *testing.T, path string) <-chan sseLine {
	t.Helper()
	req, _ := http.NewRequest("GET", g.srv.URL+path, nil)
	req.AddCookie(&http.Cookie{Name: "flip", Value: "bob"})
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the stream is read, and closed, by the goroutine below
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", path, resp.StatusCode)
	}
	out := make(chan sseLine, 1024)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				out <- sseLine{text: line}
			}
		}
		out <- sseLine{closed: true}
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return out
}

// openEvents returns once the event stream's guard is registered, which is after
// it subscribed, so an event emitted next is not lost to the gap before that.
func (g *streamRig) openEvents(t *testing.T) <-chan sseLine {
	t.Helper()
	lines := g.open(t, "/v1/sessions/s1/events")
	for range 400 {
		g.s.streamMu.Lock()
		n := len(g.s.streams)
		g.s.streamMu.Unlock()
		if n > 0 {
			return lines
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the event stream never started")
	return nil
}

// waitFor reads lines until one contains want, failing after two seconds.
func waitFor(t *testing.T, lines <-chan sseLine, want string) {
	t.Helper()
	var seen []string
	deadline := time.After(2 * time.Second)
	for {
		select {
		case l := <-lines:
			if l.closed {
				t.Fatalf("stream closed before %q; saw %q", want, seen)
			}
			seen = append(seen, l.text)
			if strings.Contains(l.text, want) {
				return
			}
		case <-deadline:
			t.Fatalf("no %q within 2s; saw %q", want, seen)
		}
	}
}

// untilClosed reads lines until the stream closes, failing after d.
func untilClosed(t *testing.T, lines <-chan sseLine, d time.Duration) []string {
	t.Helper()
	var seen []string
	deadline := time.After(d)
	for {
		select {
		case l := <-lines:
			if l.closed {
				return seen
			}
			seen = append(seen, l.text)
		case <-deadline:
			t.Fatalf("stream still open after %v; saw %q", d, seen)
		}
	}
}

func seqLine(seq int64) string { return fmt.Sprintf("id: %d", seq) }

// A sign-in that stops being accepted ends an event stream already open,
// within the recheck interval, and nothing written after it is delivered.
func TestEventStreamEndsWhenSignInIsRefusedMidStream(t *testing.T) {
	g := newStreamRig(t, flipAuth)
	lines := g.openEvents(t)
	first := g.emit(t)
	waitFor(t, lines, seqLine(first))

	g.prov.allow.Store(false)
	refusedAt := time.Now()
	// Nothing appended: the timer alone has to find the refusal.
	seen := untilClosed(t, lines, streamTestEvery+time.Second)
	if took := time.Since(refusedAt); took > streamTestEvery+500*time.Millisecond {
		t.Fatalf("stream ended %v after the refusal; the interval is %v", took, streamTestEvery)
	}
	if !strings.Contains(strings.Join(seen, "\n"), "event: refused") {
		t.Fatalf("stream closed without saying why: %q", seen)
	}

	// A new request is refused too, as before.
	req, _ := http.NewRequest("GET", g.srv.URL+"/v1/sessions/s1/events", nil)
	req.AddCookie(&http.Cookie{Name: "flip", Value: "bob"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a new stream after the refusal: %d, want 401", resp.StatusCode)
	}
}

// Events keep being written while the refusal is not yet due for a timer
// check: a write once the last check is older than the interval checks first.
func TestEventStreamChecksBeforeAStaleWrite(t *testing.T) {
	g := newStreamRig(t, flipAuth)
	lines := g.openEvents(t)
	waitFor(t, lines, seqLine(g.emit(t)))

	g.prov.allow.Store(false)
	time.Sleep(streamTestEvery + 20*time.Millisecond)
	late := g.emit(t)
	seen := untilClosed(t, lines, 2*time.Second)
	for _, l := range seen {
		if l == seqLine(late) {
			t.Fatalf("event %d, written an interval after the refusal, was delivered: %q", late, seen)
		}
	}
}

// RecheckStreams is the immediate path: once it returns, not one more event
// reaches a stream whose caller is refused, whatever the interval.
func TestRecheckStreamsStopsTheNextEvent(t *testing.T) {
	g := newStreamRig(t, flipAuth)
	g.s.opts.StreamRecheck = maxStreamRecheck
	lines := g.openEvents(t)
	waitFor(t, lines, seqLine(g.emit(t)))

	g.prov.allow.Store(false)
	g.s.RecheckStreams()
	var after []int64
	for range 5 {
		after = append(after, g.emit(t))
	}
	seen := untilClosed(t, lines, 2*time.Second)
	for _, l := range seen {
		for _, seq := range after {
			if l == seqLine(seq) {
				t.Fatalf("event %d, written after the recheck was asked for, was delivered: %q", seq, seen)
			}
		}
	}
	if !strings.Contains(strings.Join(seen, "\n"), "event: refused") {
		t.Fatalf("stream closed without saying why: %q", seen)
	}
}

// A caller still authorised keeps their stream across many rechecks.
func TestEventStreamKeepsGoingWhileAuthorised(t *testing.T) {
	for name, mw := range map[string]func(*flipProvider) *auth.Middleware{
		"signed in": flipAuth,
		"no auth":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			g := newStreamRig(t, mw)
			lines := g.openEvents(t)
			end := time.Now().Add(5 * streamTestEvery)
			for time.Now().Before(end) {
				waitFor(t, lines, seqLine(g.emit(t)))
				g.s.RecheckStreams()
				time.Sleep(streamTestEvery / 3)
			}
		})
	}
}

// Losing the session ends the stream as a refused sign-in does.
func TestEventStreamEndsWhenTheSessionIsNoLongerTheirs(t *testing.T) {
	g := newStreamRig(t, flipAuth)
	lines := g.openEvents(t)
	waitFor(t, lines, seqLine(g.emit(t)))
	g.live.mu.Lock()
	g.s.mu.Lock()
	g.live.User = "someone-else"
	g.s.mu.Unlock()
	g.live.mu.Unlock()
	g.s.RecheckStreams()
	late := g.emit(t)
	for _, l := range untilClosed(t, lines, 2*time.Second) {
		if l == seqLine(late) {
			t.Fatalf("event %d delivered after the session changed hands", late)
		}
	}
}

// startTestPTY registers a terminal run on s1 that the test writes to.
func (g *streamRig) startTestPTY() *ptyRun {
	// Output already written, so the stream answers as soon as it opens.
	run := &ptyRun{id: "p1", subs: map[chan []byte]struct{}{}, done: make(chan struct{}),
		pumped: make(chan struct{}), record: []byte("ready")}
	g.live.mu.Lock()
	g.live.ptys["p1"] = run
	g.live.mu.Unlock()
	return run
}

// said is the data line a terminal stream carries for run.say(text).
func said(text string) string {
	return "data: " + base64.StdEncoding.EncodeToString([]byte("\r\n\x1b[31m"+text+"\x1b[0m\r\n"))
}

// waitSubscribed waits until the stream is reading the run.
func waitSubscribed(t *testing.T, run *ptyRun) {
	t.Helper()
	for range 200 {
		run.mu.Lock()
		n := len(run.subs)
		run.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the terminal stream never subscribed")
}

// A terminal stream is authorised again the same way, on its timer.
func TestTerminalStreamEndsWhenSignInIsRefused(t *testing.T) {
	g := newStreamRig(t, flipAuth)
	run := g.startTestPTY()
	lines := g.open(t, "/v1/sessions/s1/pty/p1")
	waitSubscribed(t, run)
	run.say("before")
	waitFor(t, lines, said("before"))

	g.prov.allow.Store(false)
	refusedAt := time.Now()
	seen := untilClosed(t, lines, streamTestEvery+time.Second)
	if took := time.Since(refusedAt); took > streamTestEvery+500*time.Millisecond {
		t.Fatalf("terminal stream ended %v after the refusal; the interval is %v", took, streamTestEvery)
	}
	if !strings.Contains(strings.Join(seen, "\n"), "event: refused") {
		t.Fatalf("terminal stream closed without saying why: %q", seen)
	}
}

// And on RecheckStreams, before the next chunk of output.
func TestTerminalStreamStopsTheNextChunkOnRecheck(t *testing.T) {
	g := newStreamRig(t, flipAuth)
	g.s.opts.StreamRecheck = maxStreamRecheck
	run := g.startTestPTY()
	lines := g.open(t, "/v1/sessions/s1/pty/p1")
	waitSubscribed(t, run)
	run.say("before")
	waitFor(t, lines, said("before"))

	g.prov.allow.Store(false)
	g.s.RecheckStreams()
	run.say("after")
	seen := untilClosed(t, lines, 2*time.Second)
	for _, l := range seen {
		if strings.HasPrefix(l, "event: out") {
			t.Fatalf("output delivered after the recheck: %q", seen)
		}
	}
	if !strings.Contains(strings.Join(seen, "\n"), "event: refused") {
		t.Fatalf("terminal stream closed without saying why: %q", seen)
	}
}

// A still-authorised terminal keeps streaming across rechecks.
func TestTerminalStreamKeepsGoingWhileAuthorised(t *testing.T) {
	g := newStreamRig(t, flipAuth)
	run := g.startTestPTY()
	lines := g.open(t, "/v1/sessions/s1/pty/p1")
	waitSubscribed(t, run)
	end := time.Now().Add(5 * streamTestEvery)
	for time.Now().Before(end) {
		run.say("tick")
		waitFor(t, lines, said("tick"))
		g.s.RecheckStreams()
		time.Sleep(streamTestEvery / 3)
	}
}

// With local accounts, an administrator's sign-out of a user ends that user's
// open stream at once: the account layer tells the server, no interval needed.
func TestLocalSignOutEndsAnOpenStreamAtOnce(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	if err := local.CreateUser(context.Background(), auth.User{Username: "bob"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), StreamRecheck: maxStreamRecheck,
		Auth: &auth.Middleware{Providers: []auth.Provider{local},
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)}})
	user, tenant := s.callerOf(context.Background(), &auth.Identity{Subject: "bob", Tenant: "default", Provider: auth.ProviderLocal})
	s.running["s1"] = &liveSession{ID: "s1", User: user, Tenant: tenant, State: "running"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/signin",
		strings.NewReader(`{"username":"bob","password":"correct-horse-1"}`)))
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == local.CookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("sign-in: %d", rec.Code)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions/s1/events", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the defer below, once the stream has been read
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open stream: %d", resp.StatusCode)
	}
	lines := make(chan sseLine, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() != "" {
				lines <- sseLine{text: sc.Text()}
			}
		}
		lines <- sseLine{closed: true}
	}()
	emit := func(seq int64) {
		_ = s.store.Append(agent.Event{ID: fmt.Sprint("e", seq), SessionID: "s1", Seq: seq,
			Type: agent.EvAgentMessage, Payload: json.RawMessage(`{}`), CreatedAt: time.Now()})
	}
	emit(1)
	waitFor(t, lines, seqLine(1))

	local.RevokeUser("bob")
	emit(2)
	start := time.Now()
	seen := untilClosed(t, lines, 2*time.Second)
	for _, l := range seen {
		if l == seqLine(2) {
			t.Fatalf("an event written after the sign-out was delivered: %q", seen)
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the stream took %v to end; a sign-out here should end it at once", took)
	}
}

// The configured interval can only shorten the recheck, never lengthen it.
func TestStreamRecheckIsBounded(t *testing.T) {
	for _, c := range []struct{ set, want time.Duration }{
		{0, defaultStreamRecheck},
		{time.Second, time.Second},
		{time.Hour, maxStreamRecheck},
		{-time.Second, defaultStreamRecheck},
	} {
		s := New(Options{Config: config.Default(), StreamRecheck: c.set})
		if got := s.streamRecheck(); got != c.want {
			t.Errorf("StreamRecheck %v: got %v, want %v", c.set, got, c.want)
		}
	}
}
