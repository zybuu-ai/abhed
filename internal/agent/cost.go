package agent

import (
	"context"
	"fmt"

	"github.com/zybuu-ai/abhed/internal/model"
)

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
// with no price costs nothing known: the call is marked unpriced and nil.
func (l *Loop) chargeCall(name string, u model.Usage) *float64 {
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedInputTokens == 0 && u.CacheWriteTokens == 0 {
		return nil
	}
	price, ok := l.Budget.Price(name)
	if !ok {
		l.usageMu.Lock()
		l.usage.Unpriced = true
		l.usageMu.Unlock()
		return nil
	}
	c := price.Cost(u)
	l.usageMu.Lock()
	l.usage.CostUSD += c
	l.usageMu.Unlock()
	l.Budget.SpendUSD(c)
	return &c
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
	if a.USD {
		l.Budget.allowanceUSD.Store(true)
	}
	name := l.Adapter.Profile().Name
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
