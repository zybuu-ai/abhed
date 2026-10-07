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
	// session for a subagent's call. Backends that keep state per session,
	// such as the egress proxy, key it by this.
	Session string
	// Record writes one event to the session's record and reports whether
	// it was written. Nil when the command runs outside a session.
	Record func(event string, payload map[string]any) error
}

type launchKey struct{}

// WithLaunch carries l to the backend that builds the command, and to
// Abhed's own clients, whose requests are recorded for the same call.
func WithLaunch(ctx context.Context, l Launch) context.Context {
	ctx = egress.WithCaller(ctx, egress.Caller{Session: l.Session, CallID: l.CallID, Record: l.Record})
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

// Release frees at once what a backend holds for a command that never
// started, because building or starting it failed: the fence's channel,
// the call's cgroup and the wait for its launcher. A command that started
// is left alone; calling it then, or twice, does nothing.
func Release(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process != nil {
		return
	}
	if r, ok := pendingLaunches.LoadAndDelete(cmd); ok {
		r.(func())()
	}
}
