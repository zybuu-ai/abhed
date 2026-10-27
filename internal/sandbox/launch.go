package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"sync"

	"github.com/zybuu-ai/abhed/internal/loopport"

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
// any port, and connecting to ports: all of them (wide), all but Abhed's
// reserved range (loopport), or the ones named.
type loopback struct {
	on        bool
	ports     []uint16
	ephemeral bool
}

// wide reports loopback open to every port: the Mac's local services, and
// the egress proxies of other calls and sessions.
func (l loopback) wide() bool { return l.on && len(l.ports) == 0 && !l.ephemeral }

// ErrLoopbackUnderEgress refuses a command every loopback port is opened to
// while commands go out through the egress proxy, whose ports it would reach.
var ErrLoopbackUnderEgress = errors.New("sandbox: the command was not run: every loopback port is not opened while commands go out through Abhed's egress proxy")

// WithLoopback lets one command on the macOS process tier, whose network is
// otherwise closed, listen on loopback and connect to any loopback port,
// and nothing beyond. The loopback is the Mac's own, so its local services
// are reachable too, and so are other calls' egress proxies: refused while
// commands go out through the egress proxy (ErrLoopbackUnderEgress). The
// engine sets it only after the person agreed to it.
func WithLoopback(ctx context.Context) context.Context {
	return context.WithValue(ctx, loopbackKey{}, loopback{on: true})
}

// WithLoopbackEphemeral lets one command on the macOS process tier listen
// on loopback and connect to the loopback ports the system hands out to
// listeners that ask for none (loopport.Ephemeral), never one of Abhed's
// reserved range (loopport), where every Abhed listener on a Mac is: no
// egress proxy, call port or port forward of any engine is reachable, so it
// holds while commands go out through the egress proxy. It is what a
// debugger that picks its own ports needs (Delve's link to macOS's
// debugserver). The rule was never every loopback port; it is never the
// reserved range. Its profile has a rule per port, so it is written to the
// command's own folder (WithPrivateDirs), which it must have.
func WithLoopbackEphemeral(ctx context.Context) context.Context {
	return context.WithValue(ctx, loopbackKey{}, loopback{on: true, ephemeral: true})
}

// WithLoopbackPorts lets one command on the macOS process tier listen on
// loopback, and connect to these loopback ports alone: the ports a debug
// adapter and the program it debugs talk over, chosen outside the reserved
// range before it starts, which the command holds itself for its whole
// life, on 127.0.0.1 and [::1].
func WithLoopbackPorts(ctx context.Context, ports ...uint16) context.Context {
	return context.WithValue(ctx, loopbackKey{}, loopback{on: true, ports: append([]uint16(nil), ports...)})
}

func loopbackOf(ctx context.Context) loopback {
	l, _ := ctx.Value(loopbackKey{}).(loopback)
	return l
}

// FreeLoopbackPorts are n loopback ports nothing listens on now, on
// 127.0.0.1 or [::1], outside Abhed's reserved range, for a command that
// will listen on them (WithLoopbackPorts).
func FreeLoopbackPorts(n int) ([]uint16, error) {
	ps, err := loopport.FreeOutside(n)
	if err != nil {
		return nil, err
	}
	out := make([]uint16, len(ps))
	for i, p := range ps {
		out[i] = uint16(p) // #nosec G115 -- a TCP port
	}
	return out, nil
}

type privateDirKey struct{}

// WithPrivateDirs gives one command folders that only it uses. On the Linux
// process tier each is an empty tmpfs at that path in the command's own
// mount namespace, so the session's other commands, which share its /tmp
// under PrivateTemp, cannot reach what is in them, such as a debugger's
// socket. On macOS the engine makes the folder itself, readable by this
// account alone, and the command may write there and make and reach unix
// sockets there, nowhere else. Only the engine sets it.
func WithPrivateDirs(ctx context.Context, paths ...string) context.Context {
	return context.WithValue(ctx, privateDirKey{}, append(privateDirsOf(ctx), paths...))
}

func privateDirsOf(ctx context.Context) []string {
	p, _ := ctx.Value(privateDirKey{}).([]string)
	return append([]string(nil), p...)
}
