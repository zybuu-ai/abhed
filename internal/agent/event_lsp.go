package agent

// Events a person's language server records (the editor protocol's
// _abhed/lsp methods). Its requests are not recorded one by one; their count
// is in lsp.stopped, and in the metrics.
const (
	// EvLSPStarted is a language server started; see LSPStarted.
	EvLSPStarted EventType = "lsp.started"
	// EvLSPStopped is a language server ended; see LSPStopped.
	EvLSPStopped EventType = "lsp.stopped"
)

// LSPStarted is the payload of lsp.started.
type LSPStarted struct {
	LSPID    string `json:"lsp_id"`
	CallID   string `json:"call_id"`
	Language string `json:"language"`
	// Command is the server's command line, as policy judged it.
	Command string `json:"command"`
	Tier    string `json:"tier"`
	// Restart counts the restarts after a crash that came before this start.
	Restart int `json:"restart,omitempty"`
	// Purpose is "diagnostics" for a server the engine runs to give the
	// agent diagnostics after its edits; empty for a person's editor.
	Purpose string `json:"purpose,omitempty"`
	// AllowedBy, Step, Rule and Layer say what let a diagnostics server
	// start: policy (a rule or the mode) or the operator's setting
	// (tools.diagnostics on) answering a mode's ask.
	AllowedBy string `json:"allowed_by,omitempty"`
	Step      string `json:"step,omitempty"`
	Rule      string `json:"rule,omitempty"`
	Layer     string `json:"layer,omitempty"`
}

// LSPStopped is the payload of lsp.stopped.
type LSPStopped struct {
	LSPID    string `json:"lsp_id"`
	CallID   string `json:"call_id"`
	Language string `json:"language"`
	// Reason is stopped, crashed, closed or failed.
	Reason   string `json:"reason"`
	ExitCode *int   `json:"exit_code,omitempty"`
	// Requests and Refused count the editor's messages passed and refused.
	Requests   int    `json:"requests"`
	Refused    int    `json:"refused"`
	DurationMS int64  `json:"duration_ms"`
	Purpose    string `json:"purpose,omitempty"`
}
