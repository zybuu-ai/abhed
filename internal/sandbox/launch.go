package sandbox

import (
	"context"
	"net"
	"os/exec"
	"sync"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Launch is what a command's caller tells a backend that records each launch
// (the fence): the tool call it runs for, and where its events go.
type Launch struct {
	// CallID is the model's id for the tool call, as agent.CallIDOf gives it.
	CallID string
	// Session is the id of the session the call belongs to: the top-level
	// session for a subagent's call.
	Session string
	// Record writes one event to the session's record and reports whether
	// it was written. Nil when the command runs outside a session.
	Record func(event string, payload map[string]any) error
}

type launchKey struct{}

// WithLaunch carries l to the backend that builds the command, and to
// Abhed's own clients, whose requests are recorded for the same call.
func WithLaunch(ctx context.Context, l Launch) context.Context {
	ctx = egress.WithCaller(ctx, egress.Caller{Session: l.Session, CallID: l.CallID, Record: l.Record,
		Guard: egress.CallerOf(ctx).Guard})
	return context.WithValue(ctx, launchKey{}, l)
}

// LaunchOf is the Launch ctx carries, or the zero Launch.
func LaunchOf(ctx context.Context) Launch {
	l, _ := ctx.Value(launchKey{}).(Launch)
	return l
}

// pendingLaunches are the commands a backend is waiting on to start, each
// with what releases the wait: *exec.Cmd -> func().
var pendingLaunches sync.Map

// Release frees at once what a backend holds for a command that never started,
// because building or starting it failed: the fence's channel, the call's
// cgroup and the wait for its launcher.
func Release(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process != nil {
		return
	}
	if r, ok := pendingLaunches.LoadAndDelete(cmd); ok {
		r.(func())()
	}
}

type readOnlyKey struct{}

// WithReadOnlyPaths adds host paths one command sees read-only at the same
// path, on the container and process tiers: the engine's own files a
// command needs, such as the debug bridge. Only the engine sets it.
func WithReadOnlyPaths(ctx context.Context, paths ...string) context.Context {
	return context.WithValue(ctx, readOnlyKey{}, append(readOnlyPathsOf(ctx), paths...))
}

func readOnlyPathsOf(ctx context.Context) []string {
	p, _ := ctx.Value(readOnlyKey{}).([]string)
	return append([]string(nil), p...)
}

type loopbackKey struct{}

// loopback is what of the Mac's loopback one command may use: listening on
// any port, and connecting to ports, all of them (wide) or the ones named.
type loopback struct {
	on    bool
	ports []uint16
}

// wide reports loopback open to every port: the Mac's local services, and
// the egress proxies of other calls and sessions.
func (l loopback) wide() bool { return l.on && len(l.ports) == 0 }

// WithLoopback lets one command on the macOS process tier, whose network is
// otherwise closed, listen on loopback and connect to any loopback port,
// and nothing beyond. The loopback is the Mac's own, so its local services
// are reachable too, and so are other calls' egress proxies: refused under
// the egress allowlist (WithLoopbackPorts is not). The engine sets it only
// after the person agreed to it.
func WithLoopback(ctx context.Context) context.Context {
	return context.WithValue(ctx, loopbackKey{}, loopback{on: true})
}

// WithLoopbackPorts lets one command on the macOS process tier listen on
// loopback, and connect to these loopback ports alone: the ports a debug
// adapter and the program it debugs talk over, chosen before it starts. No
// other local service, nor any egress proxy, is reachable, so it holds under
// the egress allowlist too.
func WithLoopbackPorts(ctx context.Context, ports ...uint16) context.Context {
	return context.WithValue(ctx, loopbackKey{}, loopback{on: true, ports: append([]uint16(nil), ports...)})
}

func loopbackOf(ctx context.Context) loopback {
	l, _ := ctx.Value(loopbackKey{}).(loopback)
	return l
}

// FreeLoopbackPorts are n loopback ports nothing listens on now, for a
// command that will listen on them (WithLoopbackPorts).
func FreeLoopbackPorts(n int) ([]uint16, error) {
	var lns []net.Listener
	defer func() {
		for _, l := range lns {
			_ = l.Close()
		}
	}()
	var out []uint16
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		lns = append(lns, l)
		out = append(out, uint16(l.Addr().(*net.TCPAddr).Port)) // #nosec G115 -- a TCP port
	}
	return out, nil
}

type privateDirKey struct{}

// WithPrivateDirs gives one command, on the Linux process tier, empty
// folders at these paths that only it sees: each is a tmpfs in the
// command's own mount namespace, so the session's other commands, which
// share its /tmp under PrivateTemp, cannot reach what is in them, such as a
// debugger's socket. Only the engine sets it.
func WithPrivateDirs(ctx context.Context, paths ...string) context.Context {
	return context.WithValue(ctx, privateDirKey{}, append(privateDirsOf(ctx), paths...))
}

func privateDirsOf(ctx context.Context) []string {
	p, _ := ctx.Value(privateDirKey{}).([]string)
	return append([]string(nil), p...)
}
