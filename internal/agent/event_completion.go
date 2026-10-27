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
	// Requests were sent to the provider; Served answered with text; Failed
	// erred; Cancelled were sent and then cancelled. Refused were not sent (a
	// file a rule keeps, the rate limit), nor Superseded (replaced by a newer
	// request before they went).
	Requests   int `json:"requests"`
	Served     int `json:"served"`
	Refused    int `json:"refused"`
	Failed     int `json:"failed"`
	Cancelled  int `json:"cancelled"`
	Superseded int `json:"superseded"`
	// TokensIn and TokensOut are what the provider reported; a request
	// cancelled after it went may cost tokens it never reported.
	TokensIn  int `json:"tokens_in"`
	TokensOut int `json:"tokens_out"`
}
