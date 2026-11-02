package agent

// EvSandboxPortContested is more than one program listening on one of a
// listening session's listen ports, so a sandboxed command may be taking a
// program's loopback connections; Abhed tells and stops nothing. See
// SandboxPortContested.
const EvSandboxPortContested EventType = "sandbox.port_contested"

// SandboxPortContested is the payload of sandbox.port_contested: the port
// and how many processes listen on it.
type SandboxPortContested struct {
	Port      int `json:"port"`
	Listeners int `json:"listeners"`
}
