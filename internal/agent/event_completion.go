package agent

// Events an editor's inline completions record. A completion is not
// recorded one by one; the first use in a session names where the code goes,
// and a summary counts them when the session closes.
const (
	// EvCompletionEnabled is the first completion a session asked a provider for; see CompletionEnabled.
	EvCompletionEnabled EventType = "completion.enabled"
	// EvCompletionSummary counts a session's completions; see CompletionSummary.
	EvCompletionSummary EventType = "completion.summary"
	// EvNextEditEnabled and EvNextEditSummary are the same for the editor's
	// next-edit suggestions, with the same payloads.
	EvNextEditEnabled EventType = "next_edit.enabled"
	EvNextEditSummary EventType = "next_edit.summary"
	// EvCommitRequested is recorded before a staged diff goes to the commit
	// model, and EvCommitMessage when its answer is back; see CommitRequested
	// and CommitMessage.
	EvCommitRequested EventType = "commit_message.requested"
	EvCommitMessage   EventType = "commit_message.generated"
)

// CommitRequested is the payload of commit_message.requested: where the
// staged diff goes, how much of it, and what was left out.
type CommitRequested struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Local    bool   `json:"local"`
	// Files were in the diff sent; Excluded were staged but left out (a read
	// rule, a key file, Abhed's state); Bytes is what was sent, after redaction.
	Files     int  `json:"files"`
	Excluded  int  `json:"excluded"`
	Bytes     int  `json:"bytes"`
	Truncated bool `json:"truncated,omitempty"`
}

// CommitMessage is the payload of commit_message.generated.
type CommitMessage struct {
	// Outcome is served, empty or failed.
	Outcome   string `json:"outcome"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	// CostUSD is the call's cost, nil when unpriced; Estimated marks tokens
	// estimated for a call cut before the provider reported them.
	CostUSD   *float64 `json:"cost_usd,omitempty"`
	Unpriced  bool     `json:"unpriced,omitempty"`
	Estimated bool     `json:"estimated,omitempty"`
}

// CompletionEnabled is the payload of completion.enabled.
type CompletionEnabled struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Local says the provider's endpoint is on this machine.
	Local bool `json:"local"`
	// Mode is fim (a fill-in-the-middle endpoint) or chat.
	Mode string `json:"mode"`
}

// CompletionSummary is the payload of completion.summary.
type CompletionSummary struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Requests were sent to the provider; Served answered with a suggestion
	// (for next edits, renames finished from the file too: Local of them); Failed
	// erred; Cancelled were sent and then cancelled. Refused were not sent (a
	// file a rule keeps, the rate limit), nor Superseded (replaced by a newer
	// request before they went).
	Requests   int `json:"requests"`
	Served     int `json:"served"`
	Refused    int `json:"refused"`
	Failed     int `json:"failed"`
	Cancelled  int `json:"cancelled"`
	Superseded int `json:"superseded"`
	// TokensIn and TokensOut are the tokens charged: what the provider
	// reported, or, for the Estimated requests, what was estimated.
	TokensIn  int `json:"tokens_in"`
	TokensOut int `json:"tokens_out"`
	Estimated int `json:"estimated,omitempty"`
	// CostUSD is what the priced requests cost, charged to the session's
	// budget; Unpriced marks one to a model with no price.
	CostUSD  float64 `json:"cost_usd,omitempty"`
	Unpriced bool    `json:"unpriced,omitempty"`
	// Answer times of the requests sent, from Studio's request reaching the
	// engine to its answer, in milliseconds: median, 90th percentile and
	// longest. No code, only times.
	LatencyP50Ms int `json:"latency_p50_ms,omitempty"`
	LatencyP90Ms int `json:"latency_p90_ms,omitempty"`
	LatencyMaxMs int `json:"latency_max_ms,omitempty"`
	// Warmups are the empty requests that loaded a local model ahead of the
	// first completion; WarmupMs the longest of them.
	Warmups  int `json:"warmups,omitempty"`
	WarmupMs int `json:"warmup_ms,omitempty"`
	// Local are next-edit suggestions made from the file alone (a rename's
	// other uses), with no request to a model, and LocalDropped renames for
	// which none was offered (no other use, a name bound again, a new name
	// already there). Neither is in the answer times, which are of requests sent.
	Local        int `json:"local,omitempty"`
	LocalDropped int `json:"local_dropped,omitempty"`
}
