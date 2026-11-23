package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zybuu-ai/abhed/internal/model"
)

// Events of the conversation being fitted to the model's context window.
const (
	// EvContextWindow says, once a session, that the model's window is not
	// known and what Abhed assumes; see ContextWindowNote.
	EvContextWindow EventType = "context.window"
	// EvContextOverflow is a provider refusing a request as longer than the
	// window, or a server found to have cut the prompt; see ContextOverflow.
	EvContextOverflow EventType = "context.overflow"
	// EvContextRefit is the conversation re-fitted to a smaller window: after a
	// switch to a model with less room, or a window learned from the
	// provider; see ContextRefit.
	EvContextRefit EventType = "context.refit"
)

// ContextWindowNote is the payload of context.window.
type ContextWindowNote struct {
	Model  string `json:"model"`
	Window int    `json:"window"`
	// Source is where the window came from: fallback when assumed.
	Source string `json:"source"`
	Note   string `json:"note"`
}

// Actions a context.overflow records.
const (
	OverflowRetried   = "compacted_retry" // compacted and asked again
	OverflowGaveUp    = "gave_up"         // the retry overflowed too, or nothing could be made smaller
	OverflowTruncated = "truncated"       // the server cut the prompt; compacted for the next call
)

// ContextOverflow is the payload of context.overflow.
type ContextOverflow struct {
	Model string `json:"model"`
	// Window is the window in force when the request was made; Limit the
	// one the provider named, zero when it named none.
	Window int `json:"window"`
	Limit  int `json:"limit,omitempty"`
	// Sent is the estimate of what was sent; Read, for a cut prompt, what the
	// server said it read.
	Sent   int    `json:"sent,omitempty"`
	Read   int    `json:"read,omitempty"`
	Action string `json:"action"`
	// BeforeTokens and AfterTokens measure the conversation around the
	// compaction made in answer.
	BeforeTokens int    `json:"before_tokens,omitempty"`
	AfterTokens  int    `json:"after_tokens,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ContextRefit is the payload of context.refit.
type ContextRefit struct {
	// Reason is model_switch or window_learned.
	Reason     string `json:"reason"`
	FromModel  string `json:"from_model,omitempty"`
	ToModel    string `json:"to_model"`
	FromWindow int    `json:"from_window,omitempty"`
	ToWindow   int    `json:"to_window"`
	// ResultsCapped are tool results cut to the new window's share.
	ResultsCapped int  `json:"results_capped,omitempty"`
	Compacted     bool `json:"compacted,omitempty"`
	BeforeTokens  int  `json:"before_tokens"`
	AfterTokens   int  `json:"after_tokens"`
}

// unnamedLimitShare is the percentage of a refused prompt's size taken as
// the window when the refusal names no limit; see overflowRecovery.
const unnamedLimitShare = 80

// ContextOverflowError ends a run whose conversation does not fit the model
// even after compacting: it says what happened and what to do, and only what
// is known: the limit the provider named, or that it named none.
type ContextOverflowError struct {
	Model string
	// Limit is the window the provider named, zero when it named none;
	// Refused is the measured size of the refused request.
	Limit   int
	Refused int
	Err     error
}

func (e *ContextOverflowError) Error() string {
	var limit string
	switch {
	case e.Limit > 0:
		limit = fmt.Sprintf("its context window of %d tokens", e.Limit)
	case e.Refused > 0:
		limit = fmt.Sprintf("the provider did not name its limit; the refused request was about %d tokens", e.Refused)
	default:
		limit = "the provider did not name its limit"
	}
	return fmt.Sprintf("the conversation is too long for %s (%s), even after compacting it once. "+
		"Start a new conversation, compact it yourself with /compact and say what to keep, "+
		"or switch to a model with a larger window with /model. The provider said: %v", e.Model, limit, e.Err)
}

func (e *ContextOverflowError) Unwrap() error { return e.Err }

// windowRefit is a pending re-fit, set when the window shrinks between runs.
type windowRefit struct {
	reason  string
	from    model.Profile
	hasFrom bool
}

// noteWindow, on the session's first turn, asks the endpoint for its window
// and, when it is still assumed, says so once.
func (l *Loop) noteWindow(ctx context.Context) {
	if l.windowNoted || l.Adapter == nil {
		return
	}
	l.windowNoted = true
	model.Discover(ctx, l.Adapter)
	p := l.Adapter.Profile()
	if note := model.WindowNote(p); note != "" && l.depth == 0 {
		l.record(EvContextWindow, ActorSystem, ContextWindowNote{
			Model: p.Name, Window: p.ContextWindow, Source: p.WindowSource, Note: note,
		})
	}
}

// overflowRecovery answers a provider's refusal of a request as too long:
// the window is lowered to the limit it named, the conversation is made to
// fit, and the turn is asked once more. A second refusal in a row, or a
// conversation that cannot be made smaller, ends the run with what to do.
func (l *Loop) overflowRecovery(ctx context.Context, cause error, limit int) (TerminalReason, bool, error) {
	was := l.Adapter.Profile()
	info := ContextOverflow{Model: was.Name, Window: was.ContextWindow, Limit: limit, Error: truncate(cause.Error(), 600)}
	info.Sent = l.measured()
	giveUp := func() (TerminalReason, bool, error) {
		l.overflowRetried = false
		info.Action = OverflowGaveUp
		l.record(EvContextOverflow, ActorSystem, info)
		return TermError, true, &ContextOverflowError{Model: was.Name, Limit: limit, Refused: info.Sent, Err: cause}
	}
	if l.overflowRetried {
		return giveUp()
	}
	if limit > 0 {
		model.LearnWindow(l.Adapter, limit)
	} else if info.Sent > 0 {
		// No limit named: it is below what was refused, by an unknown
		// amount. Four fifths of the refused prompt puts the retry well under
		// it: compaction keeps half the window (two fifths of the refused
		// size) and caps each result at a quarter (one fifth), so the one
		// retry fits a limit as low as about half the refused size.
		model.LearnWindow(l.Adapter, info.Sent*unnamedLimitShare/100)
	}
	before, after, changed := l.shrinkToWindow(ctx, "overflow", true)
	if !changed {
		return giveUp()
	}
	l.overflowRetried = true
	// The retry asks the same turn again; it does not count as another.
	l.turns--
	info.Action, info.BeforeTokens, info.AfterTokens = OverflowRetried, before, after
	l.record(EvContextOverflow, ActorSystem, info)
	return "", false, nil
}

// measured is the conversation's size as the next request would send it,
// counted as the model counts tokens once it has reported some.
func (l *Loop) measured() int {
	req := model.Request{System: l.Config.SystemPrompt, Messages: l.messages, Tools: l.offeredDefs()}
	if l.Compactor != nil && l.Compactor.Adapter == l.Adapter {
		n, _ := l.Compactor.count(req)
		return n
	}
	n, _ := l.Adapter.CountTokens(req)
	return n
}

// noteTruncation checks a reply's reported prompt against what was sent, for
// a server that cuts an over-long prompt rather than refusing it. A cut
// prompt teaches the window, and the conversation is fitted before the next
// call; the reply already made stands, as it is in the record.
func (l *Loop) noteTruncation(ctx context.Context, sent int, u model.Usage) {
	p := l.Adapter.Profile()
	read := model.PromptTokens(u)
	if !model.TruncatedPrompt(p, sent, read) {
		return
	}
	// The loaded model's window, if the server will say; else what it read.
	if !model.Rediscover(ctx, l.Adapter) {
		model.LearnWindow(l.Adapter, read)
	}
	info := ContextOverflow{Model: p.Name, Window: p.ContextWindow, Sent: sent, Read: read, Action: OverflowTruncated,
		Limit: l.Adapter.Profile().ContextWindow}
	info.BeforeTokens, info.AfterTokens, _ = l.shrinkToWindow(ctx, "overflow", false)
	l.record(EvContextOverflow, ActorSystem, info)
}

// shrinkToWindow fits the conversation to the window now in force: every
// tool result is cut to the window's share, then the history is compacted
// when it is still over the threshold, or always when force is set. It
// reports the size before and after, and whether anything changed.
func (l *Loop) shrinkToWindow(ctx context.Context, trigger string, force bool) (before, after int, changed bool) {
	before, _ = l.contextSize()
	capped := l.recapResults()
	compacted := false
	if l.Compactor != nil {
		should := force
		if !should {
			should, _, _ = l.Compactor.ShouldCompact(l.Config.SystemPrompt, l.messages, l.offeredDefs())
		}
		if should {
			used, _ := l.contextSize()
			begun := false
			out, info, err := l.Compactor.CompactWith(l.metered(l.withCaller(ctx)), trigger, l.Config.SystemPrompt, l.messages, used, func() {
				begun = true
				l.record(EvCompactStarted, ActorSystem, Compaction{BeforeTokens: used, Trigger: trigger})
			})
			switch {
			case err != nil:
				l.record(EvCompactDone, ActorSystem, map[string]string{"error": err.Error(), "trigger": trigger})
			case len(out) != len(l.messages):
				l.messages = out
				l.usage.Compactions++
				l.justCompacted = true
				l.forgetAnswers()
				l.SubdirMemory.forget()
				l.record(EvCompactDone, ActorSystem, info)
				l.observe(ctx, HookPostCompact, "", trigger)
				compacted = true
			case begun:
				l.record(EvCompactDone, ActorSystem, map[string]string{"skipped": "no change", "trigger": trigger})
			}
		}
	}
	after, _ = l.contextSize()
	return before, after, capped > 0 || compacted
}

// recapResults cuts every tool result in history to the share of the window
// now in force, the way fitResult cuts a new one, and counts those it cut.
func (l *Loop) recapResults() int {
	limit := l.resultLimitChars()
	if limit <= 0 {
		return 0
	}
	n := 0
	for i := range l.messages {
		m := &l.messages[i]
		if m.Role != model.RoleTool || len(m.Content) <= limit || len(m.Blocks) > 0 {
			continue
		}
		m.Content = l.fitResult(m.ToolCallID, m.Content)
		n++
	}
	if n > 0 {
		l.forgetAnswers()
	}
	return n
}

// refitIfPending re-fits the conversation after the window shrank between
// runs, before the next model call: a switch to a model with less room.
func (l *Loop) refitIfPending(ctx context.Context) {
	r := l.pendingRefit
	if r == nil {
		return
	}
	l.pendingRefit = nil
	now := l.Adapter.Profile()
	info := ContextRefit{Reason: r.reason, ToModel: now.Name, ToWindow: now.ContextWindow}
	if r.hasFrom {
		info.FromModel, info.FromWindow = r.from.Name, r.from.ContextWindow
	}
	before, _ := l.contextSize()
	info.ResultsCapped = l.recapResults()
	compactions := l.usage.Compactions
	_, after, _ := l.shrinkToWindow(ctx, "model_switch", false)
	info.Compacted = l.usage.Compactions > compactions
	info.BeforeTokens, info.AfterTokens = before, after
	l.record(EvContextRefit, ActorSystem, info)
}

// calibrate compares a call's reported prompt with the estimate of what was
// sent, so the compaction check counts tokens as this model does: code,
// JSON and YAML run to more tokens a character than prose, and a newer
// tokenizer more again. The scale only ever makes the estimate larger.
func (l *Loop) calibrate(sent int, u model.Usage) {
	if l.Compactor == nil || sent < 2000 {
		return
	}
	read := model.PromptTokens(u)
	if read <= 0 || l.Adapter.Profile().SilentTruncation && model.TruncatedPrompt(l.Adapter.Profile(), sent, read) {
		return
	}
	l.Compactor.SetScale(float64(read) / float64(sent))
}

// isOverflow reports a refusal of the request as too long, and the limit named.
func isOverflow(err error) (bool, int) {
	var ce *ContextOverflowError
	if errors.As(err, &ce) {
		return false, 0
	}
	return model.ContextOverflow(err)
}

// ContextNotice is the sentence a person is shown for a context.window,
// context.overflow or context.refit event, or "" for one that says nothing
// (a run that gave up ends with its own reason).
func ContextNotice(ev Event) string {
	switch ev.Type {
	case EvContextWindow:
		var w ContextWindowNote
		if json.Unmarshal(ev.Payload, &w) == nil {
			return w.Note
		}
	case EvContextOverflow:
		var o ContextOverflow
		if json.Unmarshal(ev.Payload, &o) != nil {
			return ""
		}
		limit := o.Window
		if o.Limit > 0 {
			limit = o.Limit
		}
		switch o.Action {
		case OverflowRetried:
			return fmt.Sprintf("the conversation was too long for %s (%d tokens); compacted it from %d to %d tokens and asked again",
				o.Model, limit, o.BeforeTokens, o.AfterTokens)
		case OverflowTruncated:
			return fmt.Sprintf("%s read only %d of about %d tokens, so its window is smaller than assumed; compacted to fit",
				o.Model, o.Read, o.Sent)
		}
	case EvContextRefit:
		var f ContextRefit
		if json.Unmarshal(ev.Payload, &f) == nil {
			return fmt.Sprintf("fitted the conversation to %s's %d-token window: %d to %d tokens",
				f.ToModel, f.ToWindow, f.BeforeTokens, f.AfterTokens)
		}
	}
	return ""
}

// fitCompactor gives the compactor the loop's reply cap and size ceiling.
func (l *Loop) fitCompactor() {
	if l.Compactor != nil {
		l.Compactor.OutputTokens = l.Config.MaxTokens
		l.Compactor.Ceiling = l.Config.CompactAtTokens
	}
}
