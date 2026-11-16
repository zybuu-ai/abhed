package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
)

// PurposeCompaction, PurposePipeline and PurposeCommitMessage mark the
// model.call of a compaction summary, a pipeline's model step and a commit message.
const (
	PurposeCompaction    = "compaction"
	PurposePipeline      = "pipeline"
	PurposeCommitMessage = "commit_message" // Studio's commit message
)

// Refuse is the gate every model call passes before its request: it says why
// a call to the model named may not go out, or nil. A spent budget refuses
// any call; a dollar cap refuses a model with no price, never guessed.
func (b *Budget) Refuse(name string) error {
	if b == nil {
		return nil
	}
	if b.USDLimited() {
		if _, ok := b.Price(name); !ok {
			return fmt.Errorf("a budget of $%.2f is set and %s has no price; set one under pricing in the configuration", b.MaxUSD, name)
		}
	}
	if b.USDExhausted() {
		return fmt.Errorf("the budget of $%.2f is spent ($%.4f)", b.MaxUSD, b.SpentUSD())
	}
	if b.Exhausted() {
		return fmt.Errorf("the budget of %d tokens is spent (%d)", b.MaxTokens, b.Spent())
	}
	return nil
}

// BudgetStopped is a run stopped by its budget. Reason is "usd" for the
// dollar cap, "tokens" for the token cap, or "unpriced" for a dollar cap set
// while the model has no price, which is refused rather than guessed.
type BudgetStopped struct {
	Reason      string  `json:"reason"`
	Model       string  `json:"model,omitempty"`
	SpentUSD    float64 `json:"spent_usd,omitempty"`
	MaxUSD      float64 `json:"max_usd,omitempty"`
	SpentTokens int64   `json:"spent_tokens,omitempty"`
	MaxTokens   int64   `json:"max_tokens,omitempty"`
	Message     string  `json:"message"`
}

// chargeCall prices one call and spends its cost from the budget. A model
// with no price costs nothing known: nil, and unpriced is true.
func (l *Loop) chargeCall(name string, u model.Usage) (cost *float64, unpriced bool) {
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedInputTokens == 0 && u.CacheWriteTokens == 0 {
		return nil, false
	}
	price, ok := l.Budget.Price(name)
	if !ok {
		l.usageMu.Lock()
		l.usage.Unpriced = true
		l.usage.UnpricedCalls++
		l.usageMu.Unlock()
		return nil, true
	}
	c := price.Cost(u)
	l.usageMu.Lock()
	l.usage.CostUSD += c
	l.usageMu.Unlock()
	l.Budget.SpendUSD(c)
	return &c, false
}

// unpricedUnderCap reports a dollar cap set for a model with no price: the
// run cannot know what a call costs, so it does not make one.
func (l *Loop) unpricedUnderCap() bool {
	if !l.Budget.USDLimited() {
		return false
	}
	_, ok := l.Budget.Price(l.Adapter.Profile().Name)
	return !ok
}

// sideCall makes one model call outside the turns (critic, suggestion,
// compaction, pipeline step) through the gate, then charges and records it.
func (l *Loop) sideCall(ctx context.Context, purpose string, a model.Adapter, req model.Request) (string, model.Usage, error) {
	name := a.Profile().Name
	if err := l.Budget.Refuse(name); err != nil {
		return "", model.Usage{}, fmt.Errorf("not asked: %w", err)
	}
	turn := l.turns // read within the run: the critic, a compaction, a tool's pipeline step
	start := time.Now()
	text, usage, estimated, err := suggestCall(ctx, a, req, l.Budget.CutAtBound(a))
	mc := l.chargeSide(purpose, name, turn, usage, estimated, time.Since(start))
	if err != nil {
		mc.Error = err.Error()
	}
	l.record(EvModelCall, ActorSystem, mc)
	return text, usage, err
}

// chargeSide counts a call outside the turns in the totals and the budget,
// and returns its model.call to record. turn is taken by the caller, on the
// goroutine that owns it.
func (l *Loop) chargeSide(purpose, name string, turn int, u model.Usage, estimated bool, took time.Duration) ModelCall {
	l.usageMu.Lock()
	l.usage.InputTokens += u.InputTokens
	l.usage.OutputTokens += u.OutputTokens
	l.usage.CachedTokens += u.CachedInputTokens
	l.usage.ColdPrefillTokens += u.InputTokens - u.CachedInputTokens
	l.usageMu.Unlock()
	l.Budget.Spend(u.InputTokens + u.OutputTokens)
	mc := ModelCall{Turn: turn, Model: name, Purpose: purpose, TokensIn: u.InputTokens,
		TokensOut: u.OutputTokens, TokensCached: u.CachedInputTokens, CacheReported: u.CacheReported,
		TokensCacheWrite: u.CacheWriteTokens, LatencyMS: took.Milliseconds(), Estimated: estimated}
	mc.CostUSD, mc.Unpriced = l.chargeCall(name, u)
	return mc
}

// meterKey carries the loop whose budget a compaction's summary call is
// charged to.
type meterKey struct{}

func (l *Loop) metered(ctx context.Context) context.Context {
	return context.WithValue(ctx, meterKey{}, l)
}

// unmeteredKey marks a context whose model calls no budget covers, which only
// this package's tests set.
type unmeteredKey struct{}

// errUnmetered refuses a compaction summary with no loop's budget to charge.
var errUnmetered = errors.New("no session budget covers this call, so it was not made")

// meteredCall is a compaction summary's call, through the gate of the loop
// ctx carries. With no loop it is refused, so no caller skips the budget.
func meteredCall(ctx context.Context, purpose string, a model.Adapter, req model.Request) (string, error) {
	if l, _ := ctx.Value(meterKey{}).(*Loop); l != nil {
		text, _, err := l.sideCall(ctx, purpose, a, req)
		return text, err
	}
	if ctx.Value(unmeteredKey{}) == nil {
		return "", errUnmetered
	}
	text, _, _, err := suggestCall(ctx, a, req, false)
	return text, err
}

// recordBudgetStop records why the budget stops the run, and says so.
func (l *Loop) recordBudgetStop() {
	b := l.Budget
	stop := BudgetStopped{Model: l.Adapter.Profile().Name, SpentUSD: b.SpentUSD(), MaxUSD: b.MaxUSD,
		SpentTokens: b.Spent(), MaxTokens: b.MaxTokens}
	switch {
	case l.unpricedUnderCap():
		stop.Reason = "unpriced"
		stop.Message = fmt.Sprintf("stopped: a budget of $%.2f is set and %s has no price; set one under pricing in the configuration", b.MaxUSD, stop.Model)
	case b.USDExhausted():
		stop.Reason = "usd"
		stop.Message = fmt.Sprintf("stopped: the budget of $%.2f is spent ($%.4f)", b.MaxUSD, b.SpentUSD())
	default:
		stop.Reason = "tokens"
		stop.Message = fmt.Sprintf("stopped: the budget of %d tokens is spent (%d)", b.MaxTokens, b.Spent())
	}
	l.record(EvBudgetStopped, ActorSystem, stop)
}

// ChargeEditorCall charges a call an editor feature made (a completion, a
// next edit, a commit message) to the session's budget and cost. Those are
// not model.call events: completion.summary and commit.message carry the cost.
func (l *Loop) ChargeEditorCall(name string, u model.Usage) (cost *float64, unpriced bool) {
	l.Budget.Spend(u.InputTokens + u.OutputTokens)
	return l.chargeCall(name, u)
}

// CallEnd is how a model call ended, for what it is charged.
type CallEnd struct {
	Produced  int  // characters that came back: text, reasoning, tool calls
	Cancelled bool // cut by our own cancel or a timeout
	Failed    bool // the stream ended in an error
}

// EstimateUsage fills in what a provider did not report, or reported
// implausibly low, so a gateway that drops or shrinks usage does not make
// calls free. The whole prompt (with an Anthropic-shaped cache's reads and
// writes) under a quarter of CountTokens counts as unreported and is raised
// to it; output under a quarter of what came back, about four characters a
// token, is raised to that. A cut call (cancelled, or failed once its stream
// opened) is charged at least what came back, and when bounded (a dollar cap,
// or reasoning billed unseen) at the request's output bound: the provider
// accepted it and worked, and cutting a call does not stop its bill. Only a
// call Complete refused (no stream, so this is never asked) goes uncharged.
// estimated is true when anything was raised.
func EstimateUsage(a model.Adapter, req model.Request, u model.Usage, end CallEnd, bounded bool) (model.Usage, bool) {
	whole := u.InputTokens
	if u.CacheExcluded {
		whole += u.CachedInputTokens + u.CacheWriteTokens
	}
	estimated := false
	if counted, _ := a.CountTokens(req); whole == 0 || whole*4 < counted {
		if add := counted - whole; add > 0 {
			u.InputTokens += add
			estimated = true
		}
	}
	cut := end.Cancelled || end.Failed
	visible := (end.Produced + 3) / 4
	out := u.OutputTokens
	if cut || out*4 < visible {
		out = max(out, visible)
	}
	if bounded && cut {
		limit := req.MaxTokens
		if limit <= 0 {
			limit = a.Profile().MaxOutputTokens
		}
		out = max(out, limit)
	}
	if out > u.OutputTokens {
		u.OutputTokens, estimated = out, true
	}
	return u, estimated
}

// CutAtBound reports whether a cut call to a is charged at its output bound:
// under a dollar limit, or for a model whose reasoning is billed unseen.
func (b *Budget) CutAtBound(a model.Adapter) bool {
	p := a.Profile()
	return b.USDLimited() || p.ReasoningTokens || p.Sampling.Think
}
