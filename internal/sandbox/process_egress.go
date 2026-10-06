package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// The relay runs before anything else in any binary that links this
// package, as the fence's launcher does.
func init() {
	if len(os.Args) > 1 && os.Args[1] == egress.RelayArg {
		os.Exit(egress.RunRelay(os.Args[2:]))
	}
}

// EvEgressDecision is the record's event for one egress proxy decision.
const EvEgressDecision = egress.EventName

// Inside a Linux process-tier sandbox, the relay and the proxy's socket
// are bound at these paths.
const (
	relayDir    = "/run/abhed-egress"
	relayBinary = relayDir + "/relay"
	relaySocket = relayDir + "/proxy.sock"
)

// egressResolve looks names up for the proxy; nil is the system resolver.
// Tests replace it to point names at local servers.
var egressResolve func(ctx context.Context, host string) ([]netip.Addr, error)

// maxRoutes bounds the calls whose records a session's proxy remembers.
const maxRoutes = 4096

// egressStart starts a proxy; a test replaces it to make starting fail.
var egressStart = egress.Start

// recordFn writes one event to a record.
type recordFn = func(string, map[string]any) error

// egressSessions are the proxies of a process tier's sessions, one each, so
// a server's sessions never share a proxy, a token or a record.
type egressSessions struct {
	mu     sync.Mutex
	m      map[string]*egressState
	closed bool
}

// egressState is one session's egress proxy, started with its first
// command and stopped when the session ends or the sandbox closes.
type egressState struct {
	session string
	once    sync.Once
	proxy   *egress.Proxy
	err     error
	dir     string // the unix socket's folder, Linux only
	exe     string // this binary, run as the relay, Linux only

	mu     sync.Mutex
	routes map[string]recordFn
	own    recordFn // the record of the session's latest command
	closed bool
}

// errEgressClosed is a command built after its session's proxy, or the
// sandbox, was closed.
var errEgressClosed = errors.New("the session's sandbox is closed")

// egressRefusal says why this tier cannot hold sandbox.network allowlist,
// or "".
func egressRefusal(t Tier) string {
	return fmt.Sprintf("sandbox.network is allowlist, which only the process tier enforces (on Linux and macOS); "+
		"the %s tier is not used for it, so the network is not opened in its place", t)
}

// egressFor is the proxy of the session ctx's launch names, started if
// need be, with the launch's call remembered for its record.
func (s *Process) egressFor(ctx context.Context) (*egressState, error) {
	l := LaunchOf(ctx)
	ss := &s.egress
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return nil, errEgressClosed
	}
	if ss.m == nil {
		ss.m = map[string]*egressState{}
	}
	e := ss.m[l.Session]
	if e == nil {
		e = &egressState{session: l.Session}
		ss.m[l.Session] = e
	}
	ss.mu.Unlock()
	if err := s.startEgress(e); err != nil {
		return nil, err
	}
	e.remember(l)
	return e, nil
}

// startEgress starts e's proxy once. A session closed before or while it
// starts refuses the command: the proxy is never handed out after Close.
func (s *Process) startEgress(e *egressState) error {
	e.once.Do(func() {
		e.mu.Lock()
		closed := e.closed
		e.mu.Unlock()
		if closed {
			e.err = errEgressClosed
			return
		}
		p, err := egressStart(egress.Options{Policy: s.policy.Egress, Record: e.route, Resolve: egressResolve,
			IPv6Loopback: s.backend == "sandbox-exec"})
		if err != nil {
			e.err = err
			return
		}
		if s.backend == "bwrap" {
			exe, err := os.Executable()
			if err == nil {
				exe, err = filepath.EvalSymlinks(exe)
			}
			if err != nil {
				_ = p.Close()
				e.err = fmt.Errorf("finding this binary to run as the egress relay: %w", err)
				return
			}
			dir, err := os.MkdirTemp("", "abhed-egress-")
			if err == nil {
				err = p.ListenUnix(filepath.Join(dir, "proxy.sock"))
			}
			if err != nil {
				_ = p.Close()
				if dir != "" {
					_ = os.RemoveAll(dir)
				}
				e.err = fmt.Errorf("the egress proxy's socket: %w", err)
				return
			}
			e.dir, e.exe = dir, exe
		}
		e.proxy = p
	})
	if e.err != nil {
		return e.err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errEgressClosed
	}
	return nil
}

// route writes a decision to the record of the call it names. A call id
// this session did not launch, which a command can claim, goes to the
// session's own record marked unattributed; with no record it is dropped
// and logged. It never reaches another session: each has its own proxy
// and token.
func (e *egressState) route(ev egress.Event) {
	e.mu.Lock()
	rec := e.routes[ev.CallID]
	unattributed := rec == nil
	if unattributed {
		rec = e.own
	}
	e.mu.Unlock()
	payload := ev.Payload()
	if unattributed {
		payload["unattributed"] = true
	}
	if rec == nil {
		slog.Warn("egress decision dropped: the session has no record", "session", e.session,
			"call_id", ev.CallID, "host", ev.Host, "decision", string(ev.Decision), "rule", ev.Rule)
		return
	}
	_ = rec(EvEgressDecision, payload)
}

// remember keeps where a call's decisions are recorded.
func (e *egressState) remember(l Launch) {
	if l.Record == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.routes == nil || len(e.routes) >= maxRoutes {
		e.routes = map[string]recordFn{}
	}
	if l.CallID != "" {
		e.routes[l.CallID] = l.Record
	}
	e.own = l.Record
}

// close stops e's proxy and removes its socket's folder; commands of the
// session built after it do not start.
func (e *egressState) close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	// A proxy starting now finishes first; one never started stays so.
	e.once.Do(func() { e.err = errEgressClosed })
	var err error
	if e.proxy != nil {
		err = e.proxy.Close()
	}
	if e.dir != "" {
		err = errors.Join(err, os.RemoveAll(e.dir))
	}
	return err
}

// egressEnv is the proxy environment for a command of the call ctx names,
// starting its session's proxy if need be.
func (s *Process) egressEnv(ctx context.Context) ([]string, *egressState, error) {
	e, err := s.egressFor(ctx)
	if err != nil {
		return nil, nil, err
	}
	return e.proxy.Env(LaunchOf(ctx).CallID), e, nil
}

// port is the proxy's loopback port, or 0 with none.
func (e *egressState) port() uint16 {
	if e == nil || e.proxy == nil {
		return 0
	}
	return e.proxy.Addr().Port()
}

// relayArgs are the bwrap arguments that bind the relay and the proxy's
// socket into the sandbox, and the argv that runs the command behind the relay.
func (e *egressState) relayArgs(argv []string) (binds, wrapped []string) {
	binds = []string{"--ro-bind", e.exe, relayBinary, "--ro-bind", filepath.Join(e.dir, "proxy.sock"), relaySocket}
	listen := e.proxy.Addr().String()
	wrapped = append([]string{relayBinary, egress.RelayArg, relaySocket, listen, "--"}, argv...)
	return binds, wrapped
}

// EndSession stops the egress proxy of the session id, if it has one, and
// removes its socket's folder. A later command of that session starts a
// new proxy, with a new token.
func (s *Process) EndSession(id string) error {
	ss := &s.egress
	ss.mu.Lock()
	e := ss.m[id]
	delete(ss.m, id)
	ss.mu.Unlock()
	if e == nil {
		return nil
	}
	return e.close()
}

// Close stops every session's egress proxy and removes their sockets'
// folders. Every command built after it is refused, under the allowlist,
// rather than given a proxy that is gone.
func (s *Process) Close() error {
	ss := &s.egress
	ss.mu.Lock()
	ss.closed = true
	all := ss.m
	ss.m = nil
	ss.mu.Unlock()
	var err error
	for _, e := range all {
		err = errors.Join(err, e.close())
	}
	return err
}

// egressAvailable says why the process tier cannot enforce the allowlist
// here, or "".
func (s *Process) egressAvailable() string {
	if s.policy.Egress == nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin", "linux":
		return ""
	}
	return "sandbox.network allowlist is enforced only on Linux and macOS"
}

// egressText words the network for Describe.
func egressText(p Policy) string {
	pol := p.Egress
	mode := "enforced"
	if pol.Audit() {
		mode = "audit mode: denials are recorded, not enforced"
	}
	def := "default deny"
	if pol.DefaultAllow() {
		def = "default allow"
	}
	return fmt.Sprintf("network through the session's egress proxy only (%d rules, %s, %s; internal addresses refused unless a rule names them)",
		pol.Rules(), def, mode)
}
