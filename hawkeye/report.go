// Package hawkeye reads a session's event record and says what happened in it:
// where the tokens went, what the policy decided and at which step, what ran
// in the sandbox, what the agent touched, and what deserves a second look.
//
// It is a pure function of the record. Nothing here calls a model, reaches the
// network or reads the workspace, so a report can be produced on an air-gapped
// machine from an exported file, years after the session ran.
package hawkeye

import "time"

// Report is everything HawkEYE derives from one session.
type Report struct {
	SessionID string    `json:"session_id"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended"`
	Prompt    string    `json:"prompt"`
	// Outcome is the terminal reason, or "running" when the record has no end.
	Outcome string `json:"outcome"`

	Totals      Totals       `json:"totals"`
	Turns       []Turn       `json:"turns"`
	Calls       []Call       `json:"calls"`
	Policy      PolicyStats  `json:"policy"`
	Compactions []Compaction `json:"compactions,omitempty"`
	Offloads    []Offload    `json:"offloads,omitempty"`
	Subagents   []Subagent   `json:"subagents,omitempty"`
	Files       []FileTouch  `json:"files,omitempty"`
	Findings    []Finding    `json:"findings"`
	Integrity   Integrity    `json:"integrity"`
}

type Totals struct {
	Events       int     `json:"events"`
	Turns        int     `json:"turns"`
	ToolCalls    int     `json:"tool_calls"`
	TokensIn     int     `json:"tokens_in"`
	TokensOut    int     `json:"tokens_out"`
	TokensCached int     `json:"tokens_cached"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	// ModelMS and ToolMS split the wall clock between waiting on the model
	// and waiting on tools, which is the first question about a slow session.
	ModelMS    int64 `json:"model_ms"`
	ToolMS     int64 `json:"tool_ms"`
	DurationMS int64 `json:"duration_ms"`
	// PeakContext is the largest prompt any turn sent; Window is the limit.
	PeakContext int `json:"peak_context"`
	Window      int `json:"context_window"`
	Untrusted   int `json:"untrusted_observations"`
	// Recalls counts reads of the session's own record — the agent fetching
	// back something that had left the window.
	Recalls int `json:"recalls"`
}

// Turn is one round trip to the model.
type Turn struct {
	N            int    `json:"n"`
	Seq          int64  `json:"seq"`
	TokensIn     int    `json:"tokens_in"`
	TokensOut    int    `json:"tokens_out"`
	TokensCached int    `json:"tokens_cached"`
	Window       int    `json:"context_window"`
	CutOff       bool   `json:"cut_off,omitempty"`
	FirstTokenMS int64  `json:"first_token_ms"`
	LatencyMS    int64  `json:"latency_ms"`
	ToolCalls    int    `json:"tool_calls"`
	Error        string `json:"error,omitempty"`
}

// Call is one tool call followed through its whole life: what was asked, what
// the policy said and why, who let it through, and what came back.
type Call struct {
	Seq      int64  `json:"seq"`
	CallID   string `json:"call_id"`
	Tool     string `json:"tool"`
	Args     string `json:"args"`
	Subject  string `json:"subject"`
	Decision string `json:"decision"` // allowed | denied | pending
	Step     string `json:"step,omitempty"`
	By       string `json:"by,omitempty"` // policy | reviewer | user
	Reason   string `json:"reason,omitempty"`

	Ran        bool   `json:"ran"`
	IsError    bool   `json:"is_error"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
	Output     string `json:"output,omitempty"`
	OutputLen  int    `json:"output_len"`
}

type PolicyStats struct {
	Allowed  int            `json:"allowed"`
	Denied   int            `json:"denied"`
	Reviewer int            `json:"asked_reviewer"`
	ByStep   map[string]int `json:"by_step"`
}

type Compaction struct {
	Seq     int64  `json:"seq"`
	Trigger string `json:"trigger"`
	Before  int    `json:"before_tokens"`
	After   int    `json:"after_tokens"`
}

// Offload is one pass that moved old tool results out of the window and into
// the record. Nothing is lost by it; Recalls says how often the agent went back.
type Offload struct {
	Seq     int64 `json:"seq"`
	Results int   `json:"results"`
	Before  int   `json:"before_tokens"`
	After   int   `json:"after_tokens"`
}

type Subagent struct {
	Seq         int64  `json:"seq"`
	Description string `json:"description"`
	Type        string `json:"agent_type,omitempty"`
	Depth       int    `json:"depth"`
	Reason      string `json:"reason,omitempty"`
	Turns       int    `json:"turns"`
	TokensIn    int    `json:"tokens_in"`
	Returned    bool   `json:"returned"`
}

type FileTouch struct {
	Path   string `json:"path"`
	Reads  int    `json:"reads"`
	Writes int    `json:"writes"`
}

// Severity orders findings. Critical is reserved for the record itself being
// unreliable, because every other statement in the report depends on it.
type Severity string

const (
	Info     Severity = "info"
	Warn     Severity = "warn"
	Critical Severity = "critical"
)

type Finding struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	Seq      int64    `json:"seq,omitempty"`
}

// Integrity says whether the record looks whole. It cannot prove nothing was
// removed — only the store's append-only guarantees can — but a gap in the
// sequence is proof that something was.
type Integrity struct {
	FirstSeq int64   `json:"first_seq"`
	LastSeq  int64   `json:"last_seq"`
	Gaps     []int64 `json:"gaps,omitempty"`
	Ordered  bool    `json:"ordered"`
	HasEnd   bool    `json:"has_end"`
}
