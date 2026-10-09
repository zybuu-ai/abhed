package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/wsconn"
)

// The streaming verbs (contract §4.3): :attach, :exec with stdin, :forward
// and the :ssh relay are WebSockets. Each upgrade is refused for another
// site's page before anything is looked up (the same-origin layer passes
// every GET), then the caller must see the sandbox, and only then is the
// connection taken over; authentication ran before any of it. An open
// stream is authorised again as the session streams are, and ends when the
// sandbox stops or starts a new generation.

// Subprotocols of the streaming verbs.
const (
	protoAttach  = "abhed.attach.v1"
	protoExec    = "abhed.exec.v1"
	protoForward = "abhed.forward.v1"
	protoSSH     = "abhed.ssh.v1"
)

const (
	// maxStreamsPerCaller and maxStreams bound the streams open at once.
	maxStreamsPerCaller = 16
	maxStreams          = 512
	// streamPing is how often an idle stream is pinged; one that has sent
	// nothing for three of these is taken to be gone.
	streamPing = 20 * time.Second
	// streamMax is the longest any stream stays open: a working day.
	streamMax = 12 * time.Hour
	// streamFirstWait bounds how long :exec waits for its request.
	streamFirstWait = 30 * time.Second
)

// The subprotocols a client such as abhed sandbox offers for each stream.
const (
	AttachProtocol   = protoAttach
	ExecProtocol     = protoExec
	ForwardProtocol  = protoForward
	SSHRelayProtocol = protoSSH
)

// streamQuiet is how long a client may send nothing, pongs included,
// before its stream is taken to be gone.
var streamQuiet = 3 * streamPing

// streamLimits bound what a client sends on a stream, and how long one
// that stops reading may hold a write before its stream is closed.
var streamLimits = wsconn.Limits{MaxFrame: 1 << 20, MaxMessage: 1 << 20, WriteTimeout: 30 * time.Second}

// streamSlots counts open streams per caller, and ends them all at shutdown:
// a hijacked connection is not one the HTTP server's Shutdown closes.
type streamSlots struct {
	mu      sync.Mutex
	open    map[string]int
	total   int
	closing bool
	end     chan struct{}
}

// ending is closed when the server shuts down.
func (l *streamSlots) ending() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.end == nil {
		l.end = make(chan struct{})
	}
	return l.end
}

// closed reports whether shutdown has begun.
func (l *streamSlots) closed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closing
}

// shutdown ends every open stream and waits up to wait for them to record
// how they ended. No stream opens after it.
func (l *streamSlots) shutdown(wait time.Duration) {
	l.ending()
	l.mu.Lock()
	if !l.closing {
		l.closing = true
		close(l.end)
	}
	l.mu.Unlock()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		n := l.total
		l.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// take holds a slot for one stream, none once shutdown has begun.
func (l *streamSlots) take(principal string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open == nil {
		l.open = map[string]int{}
	}
	if l.closing || l.total >= maxStreams || l.open[principal] >= maxStreamsPerCaller {
		return nil, false
	}
	l.open[principal]++
	l.total++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.open[principal]--
			l.total--
			if l.open[principal] == 0 {
				delete(l.open, principal)
			}
		})
	}, true
}

// streamControl is a text message on a stream, either way.
type streamControl struct {
	Type       string `json:"type"`
	Generation int64  `json:"generation,omitempty"`
	Code       *int   `json:"code,omitempty"`
	Rows       uint16 `json:"rows,omitempty"`
	Cols       uint16 `json:"cols,omitempty"`
	// Error is a failure's contract code and message.
	Error *cpFault `json:"error,omitempty"`
}

func sendControl(ws *wsconn.Conn, c streamControl) error {
	b, _ := json.Marshal(c)
	return ws.WriteMessage(wsconn.Text, b)
}

// endStreamWith tells the client how the stream ended and closes it.
func endStreamWith(ws *wsconn.Conn, err error) {
	if err == nil {
		_ = ws.Close()
		return
	}
	_, code := cpStatus(err)
	_ = sendControl(ws, streamControl{Type: "error", Error: &cpFault{Code: code, Message: err.Error()}})
	_ = ws.CloseWith(wsconn.CloseNormal, code)
}

// streamOriginOK refuses an upgrade from another site's page.
func (s *Server) streamOriginOK(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || originAllowed(origin, r, s.opts.Config.Server.AllowedOrigins) {
		return true
	}
	s.log.Warn("request refused", "method", r.Method, "path", r.URL.Path, "status", http.StatusForbidden,
		"reason", "cross-origin websocket", "origin", origin, "remote", r.RemoteAddr)
	writeCPError(w, http.StatusForbidden, "forbidden", "cross-origin request rejected")
	return false
}

// streamStart is what a streaming verb has once its checks have passed.
type streamStart struct {
	sb      controlplane.Sandbox
	proto   string
	release func()
}

// openStream runs the checks every streaming verb shares, answering and
// returning false on a refusal: the origin, the caller seeing the sandbox,
// the upgrade itself, the rate limit and the open-stream bound.
func (s *Server) openStream(w http.ResponseWriter, r *http.Request, call verbCall, proto string) (streamStart, bool) {
	if !s.streamOriginOK(w, r) {
		return streamStart{}, false
	}
	sb, err := s.opts.ControlPlane.Get(r.Context(), call.caller, call.id)
	if err != nil {
		s.failCP(w, r, err)
		return streamStart{}, false
	}
	agreed, err := wsconn.Check(r, wsconn.UpgradeOptions{Protocols: []string{proto},
		CheckOrigin: func(*http.Request) bool { return true }})
	if he := new(wsconn.HandshakeError); errors.As(err, &he) {
		if he.Status == http.StatusUpgradeRequired {
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Sec-WebSocket-Version", "13")
		}
		writeCPError(w, he.Status, "invalid", he.Msg)
		return streamStart{}, false
	}
	if !cpLimit(s.cpExec, w, call.caller) {
		return streamStart{}, false
	}
	release, ok := s.cpStreams.take(call.caller.Principal)
	switch {
	case !ok && s.cpStreams.closed():
		w.Header().Set("Retry-After", "5")
		writeCPError(w, http.StatusServiceUnavailable, "unavailable", "the server is shutting down; try again")
		return streamStart{}, false
	case !ok:
		w.Header().Set("Retry-After", "30")
		writeCPError(w, http.StatusTooManyRequests, "unavailable", "too many streams are open; close one and try again")
		return streamStart{}, false
	}
	return streamStart{sb: sb, proto: agreed, release: release}, true
}

// accept takes the connection over once every check has passed.
func accept(w http.ResponseWriter, r *http.Request, proto string) (*wsconn.Conn, error) {
	ws, err := wsconn.Accept(w, r, proto, streamLimits)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", controlplane.ErrUnavailable, err)
	}
	return ws, nil
}

// watchStream ends a stream, by cancelling it, when its caller may no
// longer see the sandbox, when the sandbox stops or starts a new
// generation, when the client has gone quiet, when the server shuts down or
// this replica stops owning the sandbox, or after streamMax. It
// returns why it ended the stream, once it has stopped.
func (s *Server) watchStream(ctx context.Context, cancel context.CancelCauseFunc, r *http.Request,
	sb controlplane.Sandbox, d driver.Driver, ws *wsconn.Conn, b streamBounds) (wait func() error) {
	if b.max <= 0 {
		b.max = streamMax
	}
	guard := s.newStreamGuard(r, "", false)
	caller := s.sandboxCallerFunc()
	guard.may = func(r *http.Request) bool {
		cc, err := caller(r)
		if err != nil {
			return false
		}
		_, err = s.opts.ControlPlane.Get(r.Context(), cc, sb.ID)
		return err == nil
	}
	var ended <-chan struct{}
	if lt, ok := d.(driver.Lifetime); ok && d != nil {
		if ch, err := lt.Done(sb.ID); err == nil {
			ended = ch
		}
	}
	every := min(s.streamRecheck(), streamPing)
	tick := time.NewTicker(every)
	deadline := time.NewTimer(b.max)
	shutdown, quiet := s.cpStreams.ending(), streamQuiet
	lost := s.opts.ControlPlane.OwnershipLost(ctx, sb.ID)
	var why error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer guard.stop()
		defer tick.Stop()
		defer deadline.Stop()
		stop := func(err error) { why = err; cancel(err) }
		current := func() error {
			gen, ready, err := s.opts.ControlPlane.Current(context.WithoutCancel(ctx), sb.ID)
			switch {
			case err != nil:
				return fmt.Errorf("%w: the sandbox is gone", controlplane.ErrNotFound)
			case !ready || gen != sb.Generation:
				return fmt.Errorf("%w: the sandbox stopped or started again", controlplane.ErrInvalidTransition)
			}
			return nil
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ended:
				stop(fmt.Errorf("%w: the sandbox stopped", controlplane.ErrInvalidTransition))
				return
			case <-shutdown:
				stop(fmt.Errorf("%w: the server is shutting down", controlplane.ErrUnavailable))
				return
			case <-lost:
				stop(fmt.Errorf("%w: %w: another replica owns sandbox %s now", controlplane.ErrUnavailable, controlplane.ErrOwnershipLost, sb.ID))
				return
			case <-deadline.C:
				stop(fmt.Errorf("%w: the stream was open for %s, the longest one may be", controlplane.ErrUnavailable, b.max))
				return
			case <-guard.woken():
				if err := guard.check(); err != nil {
					stop(fmt.Errorf("%w: access ended: %w", controlplane.ErrForbidden, err))
					return
				}
			case <-tick.C:
				if err := guard.check(); err != nil {
					stop(fmt.Errorf("%w: access ended: %w", controlplane.ErrForbidden, err))
					return
				}
				if err := current(); err != nil {
					stop(err)
					return
				}
				if b.idle > 0 && b.active != nil && time.Since(b.active()) > b.idle {
					stop(fmt.Errorf("%w: nothing was sent either way for %s", controlplane.ErrUnavailable, b.idle))
					return
				}
				if ws != nil {
					if time.Since(ws.LastRead()) > quiet && (b.busy == nil || !b.busy()) {
						stop(fmt.Errorf("%w: the client stopped answering", controlplane.ErrUnavailable))
						return
					}
					_ = ws.Ping()
				}
			}
		}
	}()
	return func() error {
		cancel(nil)
		<-done
		return why
	}
}

// streamBounds are a stream's own limits; zero values take the defaults.
type streamBounds struct {
	// max is the longest the stream stays open, streamMax when zero.
	max time.Duration
	// idle, with active, ends a stream with no traffic for that long.
	idle   time.Duration
	active func() time.Time
	// busy, while true, says the server is not reading the client for
	// reasons of its own, so a quiet client is not taken to be gone.
	busy func() bool
}

// sandboxCallerFunc works out who a request acts as, as sandboxCaller does,
// for a stream checking again later.
func (s *Server) sandboxCallerFunc() func(r *http.Request) (controlplane.Caller, error) {
	return func(r *http.Request) (controlplane.Caller, error) {
		c := controlplane.Caller{Principal: UserOf(r.Context()), Admin: s.IsAdmin(r)}
		if s.opts.SandboxCaller != nil {
			return s.opts.SandboxCaller(r, c)
		}
		return c, nil
	}
}

// sandboxPolicy is what a person's commands in a sandbox are judged by: the
// server's rules, its extensions' hooks and the caller's policy layer, as
// the workbench's terminal. A layer that cannot be had refuses the call.
func (s *Server) sandboxPolicy(ctx context.Context, id string) (*policy.Engine, error) {
	pol := s.newPolicy(policy.ModeDefault)
	toolset.Police(s.opts.Extensions, pol, "sandbox:"+id)
	layer, err := s.sessionLayer(StartSpec{Tenant: TenantOf(ctx), User: UserOf(ctx)})
	if err == nil {
		err = applyLayer(pol, layer)
	}
	if err != nil {
		s.log.Error("sandbox call refused: its policy layer", "sandbox", id, "user", UserOf(ctx), "error", err)
		return nil, fmt.Errorf("%w: %w", controlplane.ErrUnavailable, err)
	}
	return pol, nil
}

// denied refuses a command line a deny rule or hook refuses. line judges it
// as a line typed at a terminal, which a shell may finish later.
func denied(pol *policy.Engine, command string, line bool) error {
	args, _ := json.Marshal(map[string]string{"command": command})
	var d policy.Result
	if line {
		d = pol.EvaluateLine("bash", true, args)
	} else {
		d = pol.Evaluate("bash", true, args)
	}
	if d.Decision != policy.Deny {
		return nil
	}
	return fmt.Errorf("%w: denied by policy: %s", controlplane.ErrForbidden, d.Reason)
}

// terminalRefusal says why this server opens no interactive shell in a
// sandbox, "" when it may: the same rules as the workbench's terminal.
func (s *Server) terminalRefusal(pol *policy.Engine) string {
	_, _, cfg := s.state.snapshot()
	switch {
	case cfg.Sandbox.Terminal == "lines":
		return "this server judges each line before it runs (sandbox.terminal is \"lines\"), which an interactive shell cannot; run each command with :exec"
	case cfg.ManagedScreens() && pol.Screens("bash"):
		return "your organisation's policy has deny rules or hooks that could refuse a command, which only a line-by-line terminal applies to every command; run each command with :exec"
	}
	return ""
}

// queryWinSize reads rows and cols from the query, 0 when absent.
func queryWinSize(r *http.Request) (driver.WinSize, error) {
	var ws driver.WinSize
	for name, dst := range map[string]*uint16{"rows": &ws.Rows, "cols": &ws.Cols} {
		v := r.URL.Query().Get(name)
		if v == "" {
			continue
		}
		n, err := strconv.ParseUint(v, 10, 16)
		if err != nil || n == 0 || n > 4096 {
			return ws, fmt.Errorf("%w: %s must be a whole number from 1 to 4096", controlplane.ErrInvalid, name)
		}
		*dst = uint16(n) // #nosec G115 -- parsed as 16 bits and bounded above
	}
	return ws, nil
}
