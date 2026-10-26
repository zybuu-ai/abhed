package sandbox

import (
	"context"
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
// path, on the container and Linux process tiers: the engine's own files a
// command needs, such as the debug bridge. Only the engine sets it.
func WithReadOnlyPaths(ctx context.Context, paths ...string) context.Context {
	return context.WithValue(ctx, readOnlyKey{}, append(readOnlyPathsOf(ctx), paths...))
}

func readOnlyPathsOf(ctx context.Context) []string {
	p, _ := ctx.Value(readOnlyKey{}).([]string)
	return append([]string(nil), p...)
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
