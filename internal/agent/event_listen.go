package agent

// EvSandboxListenerEnded is the session sandbox's commands ended because a
// program outside it started listening on a port one of them held; see
// SandboxListenerEnded.
const EvSandboxListenerEnded EventType = "sandbox.listener_ended"

// SandboxListenerEnded is the payload of sandbox.listener_ended: the port,
// the listener's process, and how many of the sandbox's processes were
// ended so the connections reach the host's program.
type SandboxListenerEnded struct {
	Port  int `json:"port"`
	PID   int `json:"pid"`
	Ended int `json:"ended"`
}
