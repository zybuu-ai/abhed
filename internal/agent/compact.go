package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/model"
)

// Compaction summarizes a conversation approaching the context limit and
// restarts from the summary (docs P3, P4).
//
// The economics matter as much as the quality here. Compaction invalidates the
// prefix cache by construction: the summarized history replaces tokens the
// serving layer had already cached, so the next turn pays cold prefill again.
// That makes compaction frequency a capacity variable, not just a quality knob,
// which is why every compaction emits an event carrying its token accounting.
type Compactor struct {
	Adapter model.Adapter
	// Threshold is the fraction of the context window at which compaction
	// fires. Below 1.0 with real margin: hitting the hard limit mid-turn is an
	// unrecoverable error, and the estimate is approximate.
	Threshold float64
	// KeepRecentTurns are preserved verbatim after the summary. The most recent
	// exchanges carry the working state the model needs to continue.
	KeepRecentTurns int
	// Headroom is the token allowance kept free for the turn that is about to
	// happen. Zero uses a quarter of the context window, matching the cap the
	// loop puts on any single tool result.
	Headroom int
	// PreCompact runs before summarization, receiving the trigger ("auto" or
	// "manual"). Operators use it to archive the full transcript before it is
	// discarded (docs §07).
	PreCompact func(trigger string, messages []model.Message) error
	// Summarizer, when set, replaces the model-written summary. It returns the
	// summary to use, or cancel to leave the history alone. This is the seam
	// the extension host plugs into: compaction is where the harness discards
	// information deliberately, and only the deployment knows what must
	// survive it.
	Summarizer func(messages []model.Message) (summary string, cancel bool)

	// focus is what the next manual compaction's summary must keep, as the
	// person asked with /compact <focus>; it is used once. See SetFocus.
	focusMu sync.Mutex
	focus   string
}

// SetFocus asks the next manual compaction to keep what focus names above
// all else. It is taken by that compaction, or cleared by SetFocus("").
func (c *Compactor) SetFocus(focus string) {
	c.focusMu.Lock()
	c.focus = strings.TrimSpace(focus)
	c.focusMu.Unlock()
}

func (c *Compactor) takeFocus() string {
	c.focusMu.Lock()
	defer c.focusMu.Unlock()
	f := c.focus
	c.focus = ""
	return f
}

func NewCompactor(a model.Adapter, threshold float64) *Compactor {
	if threshold <= 0 || threshold > 1 {
		threshold = 0.90
	}
	return &Compactor{Adapter: a, Threshold: threshold, KeepRecentTurns: 4}
}

// ShouldCompact reports whether the conversation has grown past the threshold.
//
// The threshold is applied to the used tokens PLUS headroom for what the next
// turn will add, not to used tokens alone. Without that margin the check passes
// at 24,809 of a 32,768 window, the next tool result adds 8,000, and the model
// call fails with the window exceeded — having been told, correctly, that there
// was no need to compact. The decision has to be made about the turn that is
// coming, not the one that has already happened.
func (c *Compactor) ShouldCompact(system string, messages []model.Message, tools []model.ToolDef) (bool, int, error) {
	return c.shouldCompact(system, messages, tools, true)
}

// ShouldCompactNow answers the same question with no forward allowance, for the
// check that runs after a turn's tool results have already been appended.
// Adding headroom there would count the result twice — once as history and
// again as the space it might need — and compact on almost every turn.
func (c *Compactor) ShouldCompactNow(system string, messages []model.Message, tools []model.ToolDef) (bool, int, error) {
	return c.shouldCompact(system, messages, tools, false)
}

func (c *Compactor) shouldCompact(system string, messages []model.Message,
	tools []model.ToolDef, reserve bool) (bool, int, error) {
	window := c.Adapter.Profile().ContextWindow
	if window <= 0 {
		return false, 0, nil // unknown window: never auto-compact
	}
	used, err := c.Adapter.CountTokens(model.Request{
		System: system, Messages: messages, Tools: tools,
	})
	if err != nil {
		return false, 0, err
	}
	budget := float64(used)
	if reserve {
		budget += float64(c.headroom(window))
	}
	return budget >= float64(window)*c.Threshold, used, nil
}

// headroom estimates what one more turn can add before the next check runs:
// the model's reply plus the tool results it asks for. A quarter of the window
// matches the per-result cap the loop enforces, so a turn that produces one
// maximal result still fits.
func (c *Compactor) headroom(window int) int {
	if c.Headroom > 0 {
		return c.Headroom
	}
	return window / 4
}

const summaryPrompt = `Summarize the conversation so far so that another engineer could pick up exactly where it left off.

Preserve, in this order:
1. The user's original goal, stated verbatim if short.
2. What has been done: files created or modified, with paths.
3. What was learned about the codebase that is not obvious from reading it.
4. Current state: what works, what is broken, what was just attempted.
5. The immediate next step.

Omit: tool call mechanics, file contents already written to disk, and reasoning
that led nowhere. Be specific about paths and identifiers — a vague summary
forces the work to be redone.

Write it as notes to a colleague, not prose.`

// focusLine is the person's focus for the summary, when they gave one.
func focusLine(focus string) string {
	if focus == "" {
		return ""
	}
	return "\n\nThe person asked this summary to keep, above all else: " + focus
}

// Compact replaces history with a summary plus the most recent turns.
//
// Returns the new message list and the token accounting for the event. The
// system prompt and memory file are NOT summarized — they are re-injected whole
// by the caller, since compaction discarding the operating rules is exactly the
// failure P4 warns about.
func (c *Compactor) Compact(ctx context.Context, trigger string, system string,
	messages []model.Message, beforeTokens int) ([]model.Message, Compaction, error) {
	return c.CompactWith(ctx, trigger, system, messages, beforeTokens, nil)
}

// CompactWith is Compact that calls started once there is something to
// summarise and no hook cancelled it, before the summary is written.
func (c *Compactor) CompactWith(ctx context.Context, trigger string, system string,
	messages []model.Message, beforeTokens int, started func()) ([]model.Message, Compaction, error) {

	focus := ""
	if trigger == "manual" {
		focus = c.takeFocus()
	}
	if c.PreCompact != nil {
		if err := c.PreCompact(trigger, messages); err != nil {
			return messages, Compaction{}, fmt.Errorf("pre-compact hook: %w", err)
		}
	}

	keep := c.KeepRecentTurns
	if keep < 1 {
		keep = 1
	}
	// Split at a message boundary that keeps tool calls with their results:
	// an assistant turn whose tool results were summarized away leaves the
	// model referencing a call it cannot see.
	split := boundaryBefore(messages, keep)

	// Keeping a fixed number of exchanges fails exactly when compaction is
	// needed most. Four exchanges carrying one large tool result each can
	// exceed the window on their own, and then the split lands at zero, there
	// is nothing "older" to summarize, and the no-op leaves the session to die
	// at the next model call. So the count is a preference, not a promise: drop
	// exchanges from the front until what remains actually fits.
	if window := c.Adapter.Profile().ContextWindow; window > 0 {
		// Keep as much recent history as fits in half the window, so the
		// session has room to run several more turns before the next
		// compaction. Compacting back to the threshold instead would leave it
		// one turn from compacting again — which is what produced 97
		// compactions across a 100-turn session, each one a summarizer call and
		// a discarded prefix cache.
		budget := window / 2
		for k := keep; k >= 1; k-- {
			at := boundaryBefore(messages, k)
			if at == 0 && k > 1 {
				continue // this many exchanges is the whole history; try fewer
			}
			split = at
			used, err := c.Adapter.CountTokens(model.Request{
				System: system, Messages: messages[split:],
			})
			if err != nil || used <= budget {
				break
			}
		}
	}

	older, recent := messages[:split], messages[split:]

	// One message summarised into one leaves the history the same length: the
	// previous summary, re-summarised, for a paid call that changes nothing.
	if len(older) < 2 {
		return messages, Compaction{}, nil // nothing worth summarizing yet
	}

	var summary string
	if c.Summarizer != nil {
		s, cancel := c.Summarizer(older)
		if cancel {
			return messages, Compaction{}, nil
		}
		summary = s
	}
	if started != nil {
		started()
	}
	if summary == "" {
		var err error
		summary, err = c.summarize(ctx, older, focus)
		if err != nil {
			return messages, Compaction{}, err
		}
	}

	compacted := make([]model.Message, 0, len(recent)+1)
	compacted = append(compacted, model.Message{
		Role: model.RoleUser,
		Content: "[Earlier conversation was compacted to stay within the context " +
			"window. Summary of what happened:]\n\n" + summary,
	})
	compacted = append(compacted, recent...)

	after, _ := c.Adapter.CountTokens(model.Request{System: system, Messages: compacted})
	return compacted, Compaction{
		BeforeTokens: beforeTokens,
		AfterTokens:  after,
		Summary:      summary,
		Trigger:      trigger,
	}, nil
}

func (c *Compactor) summarize(ctx context.Context, older []model.Message, focus string) (string, error) {
	// Render the history as text rather than replaying it as messages: the
	// summarizer is doing a different job than the agent, and giving it the
	// tool schemas would invite it to call them.
	var transcript strings.Builder
	for _, m := range older {
		switch m.Role {
		case model.RoleUser:
			fmt.Fprintf(&transcript, "USER: %s\n\n", m.Content)
		case model.RoleAssistant:
			if m.Content != "" {
				fmt.Fprintf(&transcript, "ASSISTANT: %s\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&transcript, "ASSISTANT called %s(%s)\n", tc.Name, truncate(string(tc.Args), 200))
			}
			transcript.WriteString("\n")
		case model.RoleTool:
			status := "result"
			if m.IsError {
				status = "ERROR"
			}
			fmt.Fprintf(&transcript, "TOOL %s: %s\n\n", status, truncate(m.Content, 400))
		}
	}

	stream, err := c.Adapter.Complete(ctx, model.Request{
		System: "You summarize engineering work accurately and concisely.",
		Messages: []model.Message{{
			Role:    model.RoleUser,
			Content: summaryPrompt + focusLine(focus) + "\n\n---\n\n" + transcript.String(),
		}},
		MaxTokens: 2048,
	})
	if err != nil {
		return "", fmt.Errorf("summarization failed: %w", err)
	}

	var out strings.Builder
	for chunk := range stream {
		switch chunk.Type {
		case model.ChunkText:
			out.WriteString(chunk.Text)
		case model.ChunkError:
			return "", fmt.Errorf("summarization failed: %w", chunk.Err)
		}
	}
	if strings.TrimSpace(out.String()) == "" {
		return "", fmt.Errorf("summarization produced no output")
	}
	return strings.TrimSpace(out.String()), nil
}

// boundaryBefore finds a split index that keeps the last `keep` exchanges and
// never separates an assistant's tool calls from their results.
//
// An exchange is an assistant turn plus whatever it produced. Counting user
// turns instead was the original approach and it made compaction a no-op on
// exactly the histories that need it: a chat has many user messages, but an
// agent session has one instruction followed by dozens of assistant/tool
// exchanges, so the scan ran to the start, returned 0, and Compact found
// nothing older to summarize. A hundred-turn session grew until the model
// refused the request.
//
// The split lands on an assistant turn, so the tool results that answer its
// calls travel with it. Splitting between a call and its result leaves the
// model reading a reply to a question it can no longer see.
func boundaryBefore(messages []model.Message, keep int) int {
	if len(messages) == 0 {
		return 0
	}
	if keep < 1 {
		keep = 1
	}
	seen := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != model.RoleAssistant {
			continue
		}
		seen++
		if seen >= keep {
			// Keep the user message that opened this exchange, when the turn
			// before it is one: an assistant reply whose prompt was summarized
			// away reads as an answer to nothing.
			if i > 0 && messages[i-1].Role == model.RoleUser {
				return i - 1
			}
			return i
		}
	}
	// Fewer exchanges than we wanted to keep. Summarize nothing rather than
	// everything: there is not enough history for a summary to be worth the
	// round trip, and Compact treats an empty older half as a no-op.
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
