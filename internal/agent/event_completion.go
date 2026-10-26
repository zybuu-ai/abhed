package agent

// Events an editor's inline completions record. A completion is not
// recorded one by one; the first use in a session names where the code goes,
// and a summary counts them when the session closes.
const (
	// EvCompletionEnabled is the first completion a session asked a provider for; see CompletionEnabled.
	EvCompletionEnabled EventType = "completion.enabled"
	// EvCompletionSummary counts a session's completions; see CompletionSummary.
	EvCompletionSummary EventType = "completion.summary"
)

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
