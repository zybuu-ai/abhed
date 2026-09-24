package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Recall lets the agent read its own session record. It is what makes
// offloading and compaction recoverable: whatever left the window is still in
// the record, and this is the way back to it.
//
// It reads one session — its own — and nothing else. The session id is fixed
// when the tool is built and is not an argument, so no instruction the model
// picks up along the way can point it at somebody else's record.
type Recall struct {
	Store     Store
	SessionID string
	// MaxChars bounds one answer. Zero means 12,000.
	MaxChars int
}

func (Recall) Name() string  { return "recall" }
func (Recall) Mutates() bool { return false }

func (Recall) Description() string {
	return "Read this session's own record. Use it when a tool result was offloaded " +
		"(its stub gives a call_id), or after a compaction, to get back exact text that " +
		"is no longer in view. Give call_id for one result in full, or query to search " +
		"everything said and returned in this session. offset pages through a long result."
}

func (Recall) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"call_id":{"type":"string","description":"The call_id from an offloaded stub. Returns that tool result."},
"query":{"type":"string","description":"Text to search for across this session's messages and tool results. Case-insensitive."},
"offset":{"type":"integer","description":"Character offset to continue a long result from. Default 0."}
}}`)
}

func (r Recall) Run(_ context.Context, _ *tools.Session, raw json.RawMessage) tools.Result {
	var a struct {
		CallID string `json:"call_id"`
		Query  string `json:"query"`
		Offset int    `json:"offset"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return tools.Result{Content: "invalid arguments: " + err.Error(), IsError: true}
	}
	if a.CallID == "" && strings.TrimSpace(a.Query) == "" {
		return tools.Result{Content: "give call_id or query", IsError: true}
	}
	if r.Store == nil {
		return tools.Result{Content: "this session has no record to read", IsError: true}
	}
	events, err := r.Store.Events(r.SessionID)
	if err != nil {
		return tools.Result{Content: "could not read the record: " + err.Error(), IsError: true}
	}
	events = withoutPersonsCalls(events)
	limit := r.MaxChars
	if limit <= 0 {
		limit = 12000
	}
	if a.CallID != "" {
		return recallOne(events, a.CallID, a.Offset, limit)
	}
	return recallSearch(events, a.Query, limit)
}

// withoutPersonsCalls drops the results of calls a person made at the
// workbench: the model never had them in its conversation, and recall is not
// a way round that.
func withoutPersonsCalls(events []Event) []Event {
	mine := map[string]bool{}
	for _, e := range events {
		if e.Type == EvActionRequested && e.Actor == ActorUser {
			var a ActionRequested
			if json.Unmarshal(e.Payload, &a) == nil {
				mine[a.CallID] = true
			}
		}
	}
	out := events[:0:0]
	for _, e := range events {
		if e.Type == EvObservation {
			var o Observation
			if json.Unmarshal(e.Payload, &o) == nil && mine[o.CallID] {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

func recallOne(events []Event, callID string, offset, limit int) tools.Result {
	for _, e := range events {
		if e.Type != EvObservation {
			continue
		}
		var o Observation
		if json.Unmarshal(e.Payload, &o) != nil || o.CallID != callID {
			continue
		}
		body := o.Content
		if offset < 0 || offset > len(body) {
			offset = 0
		}
		// Land on a character boundary whichever way the offset fell.
		for offset > 0 && offset < len(body) && !isRuneStart(body[offset]) {
			offset--
		}
		part := body[offset:]
		more := ""
		if len(part) > limit {
			part = clipRunes(part, limit)
			more = fmt.Sprintf("\n[%d more characters. Continue with offset %d.]", len(body)-offset-limit, offset+limit)
		}
		return tools.Result{
			Content:   fmt.Sprintf("Record #%d — result of %s (%d characters):\n%s%s", e.Seq, o.Tool, len(body), part, more),
			IsError:   false,
			Truncated: more != "",
		}
	}
	return tools.Result{Content: "no tool result with call_id " + callID + " in this session", IsError: true}
}

func recallSearch(events []Event, query string, limit int) tools.Result {
	needle := strings.ToLower(strings.TrimSpace(query))
	var b strings.Builder
	hits := 0
	for _, e := range events {
		text, label := searchable(e)
		if text == "" {
			continue
		}
		at := strings.Index(strings.ToLower(text), needle)
		if at < 0 {
			continue
		}
		hits++
		if b.Len() < limit {
			fmt.Fprintf(&b, "#%d %s\n  …%s…\n", e.Seq, label, oneLine(window(text, at, len(needle), 160)))
		}
	}
	if hits == 0 {
		return tools.Result{Content: fmt.Sprintf("nothing in this session's record matches %q", query)}
	}
	head := fmt.Sprintf("%d match(es) for %q. For a tool result in full, call recall with its call_id.\n", hits, query)
	return tools.Result{Content: head + b.String()}
}

// searchable returns the text of an event worth searching, and how to cite it.
func searchable(e Event) (text, label string) {
	switch e.Type {
	case EvObservation:
		var o Observation
		_ = json.Unmarshal(e.Payload, &o)
		return o.Content, fmt.Sprintf("result of %s (call_id %s)", o.Tool, o.CallID)
	case EvUserMessage:
		var m Message
		_ = json.Unmarshal(e.Payload, &m)
		return m.Text, "user"
	case EvAgentMessage:
		var m Message
		_ = json.Unmarshal(e.Payload, &m)
		return m.Text, "agent"
	}
	return "", ""
}

// window cuts text around a match, on character boundaries.
func window(s string, at, n, around int) string {
	from, to := at-around, at+n+around
	if from < 0 {
		from = 0
	}
	if to > len(s) {
		to = len(s)
	}
	for from > 0 && !isRuneStart(s[from]) {
		from--
	}
	for to < len(s) && !isRuneStart(s[to]) {
		to++
	}
	return s[from:to]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
