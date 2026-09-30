package ui

// RecordStatus says what can be claimed about the session's record.
type RecordStatus string

const (
	// RecordVerified is a durable record whose chain verified.
	RecordVerified RecordStatus = "verified"
	// RecordUnverified is a durable record that has not verified, or failed to.
	RecordUnverified RecordStatus = "unverified"
	// RecordMemory is a record held only in memory, gone when the process ends.
	RecordMemory RecordStatus = "memory"
)

// StatusModel is everything the footer, /status and a statusline command
// show about the session. The surface draws it; the session fills it in and
// passes a new one to Surface.SetStatus whenever it changes. A statusline
// command reads it as JSON on stdin, so the field names are an interface.
type StatusModel struct {
	Model    string `json:"model"`    // the model's own name
	Provider string `json:"provider"` // the configured provider name
	Mode     string `json:"mode"`
	// ModeLocked is set when the managed configuration binds the mode.
	ModeLocked bool `json:"mode_locked"`

	// ContextTokens is what the conversation occupies in the model's window,
	// and ContextPercent that as a share of the window, 0 to 100.
	ContextTokens  int `json:"context_tokens"`
	ContextPercent int `json:"context_percent"`
	// TokensIn and TokensOut are the session's totals.
	TokensIn  int `json:"tokens_in"`
	TokensOut int `json:"tokens_out"`
	// CostUSD is set only when the provider has a configured price.
	CostUSD *float64 `json:"cost_usd,omitempty"`

	// BackgroundTasks is how many background tasks are running.
	BackgroundTasks int `json:"background_tasks"`
	// WaitingAsk is set while an approval or a question waits on the person.
	WaitingAsk bool `json:"waiting_ask"`
	// SandboxTier is none, process, container or vm; Network is whether the
	// sandbox lets commands reach the network.
	SandboxTier string `json:"sandbox_tier"`
	Network     bool   `json:"network"`

	Record      RecordStatus `json:"record"`
	SessionName string       `json:"session_name,omitempty"`
	GitBranch   string       `json:"git_branch,omitempty"`
}
