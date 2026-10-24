package agent

// Events a person's own choice of a local Ollama model records: starting the
// Ollama server, installing Ollama, and pulling a model. The agent cannot
// cause any of them; only a person's model selection in the CLI, the
// workbench, the console or Studio can, and each attempt is recorded with
// how it ended.
const (
	// EvOllamaStart is an attempt to start the local Ollama server; see OllamaAction.
	EvOllamaStart EventType = "ollama.start"
	// EvOllamaInstall is an attempt to install Ollama; see OllamaAction.
	EvOllamaInstall EventType = "ollama.install"
	// EvOllamaPull is an attempt to download a model into Ollama; see OllamaAction.
	EvOllamaPull EventType = "ollama.pull"
)

// Outcomes of an OllamaAction.
const (
	OllamaOK        = "ok"        // done
	OllamaFailed    = "failed"    // tried and failed; Error and Problem say why
	OllamaDeclined  = "declined"  // the person said no when asked
	OllamaRefused   = "refused"   // not allowed here: assist off, not an administrator, not this machine
	OllamaCancelled = "cancelled" // stopped part way by the person
	OllamaManual    = "manual"    // the step is the person's to finish, such as an installer page opened
)

// OllamaAction is the payload of ollama.start, ollama.install and
// ollama.pull: what was attempted, by whom, through which surface, and how
// it ended.
type OllamaAction struct {
	// Op is start, install or pull.
	Op string `json:"op"`
	// Outcome is ok, failed, declined, refused, cancelled or manual.
	Outcome string `json:"outcome"`
	// Provider is the configured provider name the person chose.
	Provider string `json:"provider,omitempty"`
	// Model is the Ollama model tag.
	Model string `json:"model,omitempty"`
	// Base is the Ollama API address, always this machine's loopback for an
	// action that ran.
	Base string `json:"base,omitempty"`
	// Command is the exact command run or opened, as the person was shown it.
	Command string `json:"command,omitempty"`
	// Source is where an install or a pull comes from.
	Source string `json:"source,omitempty"`
	// Bytes is a pull's download size, or an installer's when known.
	Bytes int64 `json:"bytes,omitempty"`
	// PID is a started server's process id.
	PID int `json:"pid,omitempty"`
	// Consent is how the person agreed: asked (this time), always (a
	// standing choice to start), or empty when nothing ran.
	Consent string `json:"consent,omitempty"`
	// Problem is the kind of failure or refusal, as ollama.Problem names it.
	Problem string `json:"problem,omitempty"`
	Error   string `json:"error,omitempty"`
	// By is user: only a person starts any of these.
	By string `json:"by"`
	// Surface is cli, tui, server or acp.
	Surface string `json:"surface,omitempty"`
}
