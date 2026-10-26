package agent

// Events a person's debug session records (docs/guide/16-workbench.md and
// the editor protocol's _abhed/debug methods). The launch itself is the
// person's bash call before it, with its decision and observation.
const (
	// EvDebugLaunched is a debug session started; see DebugLaunched.
	EvDebugLaunched EventType = "debug.launched"
	// EvDebugStopped is a debug session ended; see DebugStopped.
	EvDebugStopped EventType = "debug.stopped"
)

// DebugLaunched is the payload of debug.launched.
type DebugLaunched struct {
	DebugID  string `json:"debug_id"`
	CallID   string `json:"call_id"`
	Language string `json:"language"`
	// Command is the debuggee's command line, as policy judged it.
	Command string `json:"command"`
	Tier    string `json:"tier"`
	// Bridge names the socket bridge Delve's session runs through, and
	// whether its checksum was pinned by the build.
	Bridge       string `json:"bridge,omitempty"`
	BridgePinned bool   `json:"bridge_pinned,omitempty"`
}

// DebugStopped is the payload of debug.stopped.
type DebugStopped struct {
	DebugID    string `json:"debug_id"`
	CallID     string `json:"call_id"`
	Reason     string `json:"reason"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}
