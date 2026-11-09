package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
)

// PurposeCompaction and PurposePipeline mark the model.call of a compaction
// summary and of a pipeline's model step.
const (
	PurposeCompaction = "compaction"
	PurposePipeline   = "pipeline"
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
	start := time.Now()
	text, usage, err := suggestCall(ctx, a, req)
	mc := l.chargeSide(purpose, name, usage, time.Since(start))
	if err != nil {
		mc.Error = err.Error()
	}
	l.record(EvModelCall, ActorSystem, mc)
	return text, usage, err
}

// chargeSide counts a call outside the turns in the totals and the budget,
// and returns its model.call to record.
func (l *Loop) chargeSide(purpose, name string, u model.Usage, took time.Duration) ModelCall {
	l.usageMu.Lock()
	l.usage.InputTokens += u.InputTokens
	l.usage.OutputTokens += u.OutputTokens
	l.usage.CachedTokens += u.CachedInputTokens
	l.usage.ColdPrefillTokens += u.InputTokens - u.CachedInputTokens
	l.usageMu.Unlock()
	l.Budget.Spend(u.InputTokens + u.OutputTokens)
	mc := ModelCall{Turn: l.turns, Model: name, Purpose: purpose, TokensIn: u.InputTokens,
		TokensOut: u.OutputTokens, TokensCached: u.CachedInputTokens, CacheReported: u.CacheReported,
		TokensCacheWrite: u.CacheWriteTokens, LatencyMS: took.Milliseconds()}
	mc.CostUSD, mc.Unpriced = l.chargeCall(name, u)
	return mc
}

// meterKey carries the loop whose budget a compaction's summary call is
// charged to.
type meterKey struct{}

func (l *Loop) metered(ctx context.Context) context.Context {
	return context.WithValue(ctx, meterKey{}, l)
}

// meteredCall is a compaction summary's call: through the gate of the loop
// ctx carries, or straight to the adapter outside one.
func meteredCall(ctx context.Context, purpose string, a model.Adapter, req model.Request) (string, error) {
	if l, _ := ctx.Value(meterKey{}).(*Loop); l != nil {
		text, _, err := l.sideCall(ctx, purpose, a, req)
		return text, err
	}
	text, _, err := suggestCall(ctx, a, req)
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
