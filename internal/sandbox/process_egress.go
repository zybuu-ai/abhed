package sandbox

import (
	"context"
	"errors"
	"fmt"
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

// maxRoutes bounds the calls whose records the proxy remembers.
const maxRoutes = 4096

// egressState is a process-tier session's egress proxy, started with its
// first command and stopped by Close.
type egressState struct {
	once  sync.Once
	proxy *egress.Proxy
	err   error
	dir   string // the unix socket's folder, Linux only
	exe   string // this binary, run as the relay, Linux only

	mu     sync.Mutex
	routes map[string]func(string, map[string]any) error
	last   func(string, map[string]any) error
	closed bool
}

// egressRefusal says why this tier cannot hold sandbox.network allowlist,
// or "".
func egressRefusal(t Tier) string {
	return fmt.Sprintf("sandbox.network is allowlist, which only the process tier enforces (on Linux and macOS); "+
		"the %s tier is not used for it, so the network is not opened in its place", t)
}

// startEgress starts the session's proxy once.
func (s *Process) startEgress() (*egress.Proxy, error) {
	e := &s.egress
	e.once.Do(func() {
		e.mu.Lock()
		closed := e.closed
		e.mu.Unlock()
		if closed {
			e.err = errors.New("the session's sandbox is closed")
			return
		}
		p, err := egress.Start(egress.Options{Policy: s.policy.Egress, Record: e.route, Resolve: egressResolve})
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
	return e.proxy, e.err
}

// route writes a decision to the record of the call it names, or the
// session's last known record when the call is not one launched here.
func (e *egressState) route(ev egress.Event) {
	e.mu.Lock()
	rec := e.routes[ev.CallID]
	if rec == nil {
		rec = e.last
	}
	e.mu.Unlock()
	if rec != nil {
		_ = rec(EvEgressDecision, ev.Payload())
	}
}

// remember keeps where a call's decisions are recorded.
func (e *egressState) remember(l Launch) {
	if l.Record == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.routes == nil || len(e.routes) >= maxRoutes {
		e.routes = map[string]func(string, map[string]any) error{}
	}
	if l.CallID != "" {
		e.routes[l.CallID] = l.Record
	}
	e.last = l.Record
}

// egressEnv is the proxy environment for a command of the call ctx names,
// starting the proxy if need be.
func (s *Process) egressEnv(ctx context.Context) ([]string, error) {
	p, err := s.startEgress()
	if err != nil {
		return nil, err
	}
	l := LaunchOf(ctx)
	s.egress.remember(l)
	return p.Env(l.CallID), nil
}

// egressPort is the proxy's loopback port, or 0 before it starts.
func (s *Process) egressPort() uint16 {
	if s.egress.proxy == nil {
		return 0
	}
	return s.egress.proxy.Addr().Port()
}

// relayArgs are the bwrap arguments that bind the relay and the proxy's
// socket into the sandbox, and the argv that runs the command behind the relay.
func (s *Process) relayArgs(argv []string) (binds, wrapped []string) {
	e := &s.egress
	binds = []string{"--ro-bind", e.exe, relayBinary, "--ro-bind", filepath.Join(e.dir, "proxy.sock"), relaySocket}
	listen := e.proxy.Addr().String()
	wrapped = append([]string{relayBinary, egress.RelayArg, relaySocket, listen, "--"}, argv...)
	return binds, wrapped
}

// Close stops the session's egress proxy, if one was started; commands
// built after it do not start.
func (s *Process) Close() error {
	e := &s.egress
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	// A proxy starting now finishes first; one never started stays so.
	e.once.Do(func() { e.err = errors.New("the session's sandbox is closed") })
	var err error
	if e.proxy != nil {
		err = e.proxy.Close()
	}
	if e.dir != "" {
		err = errors.Join(err, os.RemoveAll(e.dir))
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
