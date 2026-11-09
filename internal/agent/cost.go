package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	if m := b.allowanceStop.Load(); m != nil && *m != "" {
		return errors.New(strings.TrimPrefix(*m, "stopped: "))
	}
	if b.USDLimited() {
		if _, ok := b.Price(name); !ok {
			if b.MaxUSD <= 0 {
				return fmt.Errorf("a dollar budget applies and %s has no price; set one under pricing in the configuration", name)
			}
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
// dollar cap, "tokens" for the token cap, "allowance" for a limit kept
// outside the session (Scope names it), or "unpriced" for a dollar cap or
// limit set while the model has no price, which is refused rather than guessed.
type BudgetStopped struct {
	Reason      string  `json:"reason"`
	Scope       string  `json:"scope,omitempty"`
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

// unpricedUnderCap reports a dollar limit (the session's cap, or an
// allowance's) set for a model with no price: the run cannot know what a
// call costs, so it does not make one.
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
	text, usage, estimated, err := suggestCall(ctx, a, req)
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
	text, _, _, err := suggestCall(ctx, a, req)
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
		if b.MaxUSD <= 0 {
			stop.Message = fmt.Sprintf("stopped: a dollar budget applies and %s has no price; set one under pricing in the configuration", stop.Model)
		}
	case b.USDExhausted():
		stop.Reason = "usd"
		stop.Message = fmt.Sprintf("stopped: the budget of $%.2f is spent ($%.4f)", b.MaxUSD, b.SpentUSD())
	default:
		stop.Reason = "tokens"
		stop.Message = fmt.Sprintf("stopped: the budget of %d tokens is spent (%d)", b.MaxTokens, b.Spent())
	}
	l.record(EvBudgetStopped, ActorSystem, stop)
}

// Allowance is what a limit kept outside the session says at a turn
// boundary, such as an edition's periodic budget for the session's owner.
type Allowance struct {
	// Stop ends the run as budget.stopped, with Message saying why.
	Stop bool
	// USD marks a dollar limit in force, so a model with no price is refused.
	USD bool
	// Scope names the limit for the record, such as "user bob, monthly".
	Scope   string
	Message string
}

// allowanceStop asks the outside limit, if any, whether the run may take
// another turn, and records why not.
func (l *Loop) allowanceStop(ctx context.Context) bool {
	if l.Budget == nil || l.Budget.Allowance == nil {
		return false
	}
	a := l.Budget.Allowance(ctx)
	if ctx.Err() != nil {
		return false // interrupted while asking: the run ends as stopped, not over budget
	}
	if a.USD {
		l.Budget.allowanceUSD.Store(true)
	}
	name := l.Adapter.Profile().Name
	// The gate every other call passes reads the last answer: a spent
	// allowance refuses a suggestion, the critic or a compaction too.
	held := ""
	if a.Stop {
		held = a.Message
		if held == "" {
			held = "stopped: the budget is spent"
		}
	}
	l.Budget.allowanceStop.Store(&held)
	stop := BudgetStopped{Reason: "allowance", Scope: a.Scope, Model: name, Message: a.Message}
	switch {
	case a.Stop:
		if stop.Message == "" {
			stop.Message = "stopped: the budget is spent"
		}
	case a.USD:
		if _, ok := l.Budget.Price(name); ok {
			return false
		}
		stop.Reason = "unpriced"
		stop.Message = fmt.Sprintf("stopped: a dollar budget applies (%s) and %s has no price; set one under pricing in the configuration", a.Scope, name)
	default:
		return false
	}
	l.record(EvBudgetStopped, ActorSystem, stop)
	return true
}

// ChargeEditorCall charges a call an editor feature made (a completion, a
// next edit, a commit message) to the session's budget and cost. Those are
// not model.call events: completion.summary and commit.message carry the cost.
func (l *Loop) ChargeEditorCall(name string, u model.Usage) (cost *float64, unpriced bool) {
	l.Budget.Spend(u.InputTokens + u.OutputTokens)
	return l.chargeCall(name, u)
}
