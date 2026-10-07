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
	"strconv"
	"strings"
	"sync"
	"time"

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
	relayDNS    = relayDir + "/dns.sock"
)

// egressResolve looks names up for the proxy; nil is the system resolver.
// Tests replace it to point names at local servers.
var egressResolve func(ctx context.Context, host string) ([]netip.Addr, error)

// maxRoutes bounds the calls whose records a session's proxy remembers.
const maxRoutes = 4096

// egressStart starts a proxy; a test replaces it to make starting fail.
var egressStart = egress.Start

// egressQuiet is how long a session's proxy outlives its last command: a
// session left open but idle holds no listener, goroutine or socket folder.
const egressQuiet = 30 * time.Second

// recordFn writes one event to a record.
type recordFn = func(string, map[string]any) error

// egressSessions are the proxies of a process tier's sessions, one each, so
// a server's sessions never share a proxy, a token or a record.
type egressSessions struct {
	mu     sync.Mutex
	m      map[string]*egressState
	closed bool
	// quiet overrides egressQuiet; tests shorten it.
	quiet time.Duration
}

// egressState is one session's egress proxy; it stops with the session, the
// sandbox, or after egressQuiet with no command in flight.
type egressState struct {
	session string
	// Under egressSessions.mu: quiet changes per command, so only the latest wait closes the proxy.
	inflight int
	quiet    uint64
	once     sync.Once
	proxy    *egress.Proxy
	err      error
	dir      string // the unix socket's folder, Linux only
	exe      string // this binary, run as the relay, Linux only
	dns      bool   // the session's resolver is served, Linux only

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

// egressFor is the session's proxy, started if need be, and a credential for the launch's
// call (mcp/<name> too) that lives exactly as long as ctx: one never cancelled keeps it all session.
func (s *Process) egressFor(ctx context.Context) (*egressState, *egress.Call, error) {
	l := LaunchOf(ctx)
	ss := &s.egress
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return nil, nil, errEgressClosed
	}
	if ss.m == nil {
		ss.m = map[string]*egressState{}
	}
	e := ss.m[l.Session]
	if e == nil {
		e = &egressState{session: l.Session}
		ss.m[l.Session] = e
	}
	e.inflight++
	e.quiet++
	ss.mu.Unlock()
	// A command's context ends when it does; one that never ends keeps the
	// proxy until the session ends.
	context.AfterFunc(ctx, func() { s.egressDone(e) })
	if err := s.startEgress(e); err != nil {
		return nil, nil, err
	}
	call, err := e.proxy.Issue(l.CallID)
	if err != nil {
		return nil, nil, err
	}
	// The command's ctx outlives it when it runs on in the background, so the credential does too.
	context.AfterFunc(ctx, call.End)
	e.remember(l)
	return e, call, nil
}

// egressDone notes that a command of e's session ended, and closes the
// proxy once the session has run none for egressQuiet.
func (s *Process) egressDone(e *egressState) {
	ss := &s.egress
	ss.mu.Lock()
	defer ss.mu.Unlock()
	e.inflight--
	if e.inflight > 0 || ss.m[e.session] != e {
		return
	}
	gen, wait := e.quiet, ss.quiet
	if wait <= 0 {
		wait = egressQuiet
	}
	time.AfterFunc(wait, func() { s.closeIfQuiet(e, gen) })
}

// closeIfQuiet closes e's proxy if no command was built since wait gen began;
// removed under the lock first, so a new command never gets a closing proxy.
func (s *Process) closeIfQuiet(e *egressState, gen uint64) {
	ss := &s.egress
	ss.mu.Lock()
	if ss.m[e.session] != e || e.inflight > 0 || e.quiet != gen {
		ss.mu.Unlock()
		return
	}
	delete(ss.m, e.session)
	ss.mu.Unlock()
	if err := e.close(); err != nil {
		slog.Warn("closing a quiet session's egress proxy", "session", e.session, "err", err)
	}
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
			if s.bwrapDNSOK(exe) {
				if err := startResolver(p, dir); err != nil {
					_ = p.Close()
					_ = os.RemoveAll(dir)
					e.err = fmt.Errorf("the egress resolver: %w", err)
					return
				}
				e.dns = true
			}
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

// route writes a decision to its call's record, else the session's latest;
// with neither it is dropped and logged.
func (e *egressState) route(ev egress.Event) {
	e.mu.Lock()
	rec := e.routes[ev.CallID]
	if rec == nil {
		rec = e.own
	}
	e.mu.Unlock()
	payload := ev.Payload()
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
	e, call, err := s.egressFor(ctx)
	if err != nil {
		return nil, nil, err
	}
	return call.Env(), e, nil
}

// port is the proxy's loopback port, or 0 with none.
func (e *egressState) port() uint16 {
	if e == nil || e.proxy == nil {
		return 0
	}
	return e.proxy.Addr().Port()
}

// relayArgs are the bwrap arguments binding the relay, the proxy's socket and, with the
// resolver, its socket and resolv.conf; and the argv running the command behind the relay.
func (e *egressState) relayArgs(argv []string) (binds, wrapped []string) {
	binds = []string{"--ro-bind", e.exe, relayBinary, "--ro-bind", filepath.Join(e.dir, "proxy.sock"), relaySocket}
	listen := e.proxy.Addr().String()
	wrapped = []string{relayBinary, egress.RelayArg, relaySocket, listen}
	if e.dns {
		// Bound after the host's resolv.conf, so this one is what the command reads.
		binds = append(binds, "--ro-bind", filepath.Join(e.dir, "dns.sock"), relayDNS,
			"--ro-bind", filepath.Join(e.dir, "resolv.conf"), "/etc/resolv.conf")
		binds = append(binds, relayCaps()...)
		wrapped = append(wrapped, egress.DNSArg, relayDNS, strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid()))
	}
	return binds, append(append(wrapped, "--"), argv...)
}

// relayCaps let the relay bind port 53: as root of the namespace that owns the network,
// since bwrap's own nesting would leave it none there; the relay nests the command itself.
func relayCaps() []string {
	// As root the command is not nested, so the relay empties the bounding set (SETPCAP) before it starts.
	if os.Getuid() == 0 {
		return []string{"--cap-drop", "ALL", "--cap-add", "CAP_NET_BIND_SERVICE", "--cap-add", "CAP_SETPCAP"}
	}
	return []string{"--uid", "0", "--gid", "0", "--cap-add", "CAP_NET_BIND_SERVICE", "--cap-add", "CAP_SETFCAP"}
}

// resolvConf points a sandbox's lookups at the relay's resolver, and nowhere else; "search ."
// stops glibc appending the hostname's domain, which would query, and deny, name.localdomain.
const resolvConf = "# Written by Abhed: names resolve through the session's egress policy.\nnameserver 127.0.0.1\nsearch .\noptions timeout:2 attempts:2\n"

// startResolver serves p's resolver beside its proxy socket in dir and writes
// the resolv.conf bound over the sandbox's; the host's own file is not touched.
func startResolver(p *egress.Proxy, dir string) error {
	if err := p.ListenDNS(filepath.Join(dir, "dns.sock")); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "resolv.conf"), []byte(resolvConf), 0o644) // #nosec G306 -- read inside the sandbox
}

// bwrapDNSOK reports whether the relay can serve the resolver here: bind port
// 53 and start a command as this user. Without it names stay unresolved.
func (s *Process) bwrapDNSOK(exe string) bool {
	s.dnsOnce.Do(func() {
		if !s.bwrapFreshOK() { // the relay writes the command's id maps through /proc
			slog.Warn("egress: no private /proc here, so names will not resolve inside the sandbox")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		args := append([]string{"--die-with-parent", "--unshare-net", "--unshare-pid", "--ro-bind", "/", "/",
			"--proc", "/proc", "--dev", "/dev"}, relayCaps()...)
		// The command must run as this user and hold no capability, or the resolver stays off.
		sets := "Inh|Prm|Eff|Amb"
		if os.Getuid() == 0 {
			sets += "|Bnd" // a root command would regain its bounding set at exec
		}
		check := fmt.Sprintf(`test "$(id -u)" = %d && ! grep -qE '^Cap(%s):.*[1-9a-f]' /proc/self/status`, os.Getuid(), sets)
		args = append(args, "--setenv", "HTTP_PROXY", "http://probe:probe@127.0.0.1:1", exe, egress.RelayArg, "/nonexistent", "127.0.0.1:0",
			egress.DNSArg, "/nonexistent", strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid()), "--", "/bin/sh", "-c", check)
		out, err := bwrapRun(ctx, args...)
		s.dnsOK = err == nil
		if err != nil {
			slog.Warn("egress: the relay cannot serve the resolver here, so names will not resolve inside the sandbox",
				"err", err, "output", strings.TrimSpace(string(out)))
		}
	})
	return s.dnsOK
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
