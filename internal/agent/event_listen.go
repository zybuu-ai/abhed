package agent

// EvSandboxListenerEnded is a sandboxed command's listener ended because a
// program outside the sandbox started listening on its port too; see
// SandboxListenerEnded.
const EvSandboxListenerEnded EventType = "sandbox.listener_ended"

// SandboxListenerEnded is the payload of sandbox.listener_ended: the port,
// and the sandboxed process ended so the connections reach the host's.
type SandboxListenerEnded struct {
	Port int `json:"port"`
	PID  int `json:"pid"`
}
