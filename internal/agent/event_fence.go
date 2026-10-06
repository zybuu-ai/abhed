package agent

import (
	"context"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// Events of the fence tier (sandbox.tier "fence"), written by the system.
const (
	// EvFenceQualified is the fence's probe report and what the session's
	// commands get, recorded at the session's start.
	EvFenceQualified EventType = "fence.qualified"
	// EvProcessLaunched is a fenced command's launch, with its call id and
	// what the launcher applied, recorded before the command runs.
	EvProcessLaunched EventType = "process.launched"
	// EvFenceLimit is a fenced command that ran into its memory or process
	// limit, recorded once it has exited.
	EvFenceLimit EventType = "fence.limit"
	// EvFenceStatePlanted is a .abhed a fenced command made in the
	// workspace: where it was moved, and that the session's fence runs no
	// further command.
	EvFenceStatePlanted EventType = sandbox.EvFenceStatePlanted
	// EvEgressDecision is one decision of the egress proxy under
	// sandbox.network allowlist: the call, host, port, address, method and
	// path for plain HTTP, the decision, the rule, and the bytes each way.
	EvEgressDecision EventType = sandbox.EvEgressDecision
)

// SandboxRecord is rec as a sandbox writes to it: system events, trusted.
func SandboxRecord(rec *Recorder) func(event string, payload map[string]any) error {
	return func(event string, payload map[string]any) error {
		_, err := rec.Record(EventType(event), ActorSystem, Trusted, payload)
		return err
	}
}

// LaunchContext is withLaunch for a person's own call that a surface runs
// itself, such as a ! command, so its fenced launch is recorded as well.
func (l *Loop) LaunchContext(ctx context.Context, callID string) context.Context {
	return l.withLaunch(ctx, callID)
}

// withLaunch names the call a command runs for, and gives the sandbox the
// loop's record for the launch events it writes.
func (l *Loop) withLaunch(ctx context.Context, callID string) context.Context {
	ctx = WithCallID(ctx, callID)
	return sandbox.WithLaunch(ctx, sandbox.Launch{CallID: CallIDOf(ctx), Record: SandboxRecord(l.Recorder)})
}
