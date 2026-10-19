package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Plan decisions, as plan.decided records them.
const (
	PlanAccepted     = "accept"
	PlanKeepPlanning = "keep-planning"
)

// planState is the plan proposed in the current run, waiting for the person.
type planState struct {
	mu      sync.Mutex
	pending *PlanProposed
	// offered is the runbook of the plan last proposed, until it is accepted
	// or another is; bound is the one accepted, which calls are held to.
	offered *Runbook
	bound   *boundPlan
}

// EnablePlanExit gives the loop the exit_plan tool, offered only in plan
// mode. A plan it takes is recorded as plan.proposed and held for TakePlan;
// the tool itself changes neither the mode nor anything else.
func (l *Loop) EnablePlanExit() {
	if l.Tools == nil {
		l.Tools = tools.NewRegistry()
	} else {
		// A registry can be shared by sessions; this one's tool goes on a copy.
		l.Tools = l.Tools.Clone()
	}
	l.Tools.Add(tools.ExitPlan{Submit: l.proposePlan})
}

func (l *Loop) proposePlan(_ context.Context, text string, steps []tools.RunbookStep) error {
	rb, err := l.prepareRunbook(text, steps)
	if err != nil {
		return fmt.Errorf("the runbook was refused: %w", err)
	}
	p := PlanProposed{Text: text, Runbook: rb, Hash: planTextHash(text)}
	if rb != nil {
		p.Hash = rb.Hash
	}
	if _, err := l.Recorder.Record(EvPlanProposed, ActorAgent, Trusted, p); err != nil {
		return err
	}
	l.plans.mu.Lock()
	l.plans.pending = &p
	l.plans.offered = rb
	l.plans.mu.Unlock()
	return nil
}

// PendingPlan returns the plan the last run proposed, leaving it for TakePlan.
func (l *Loop) PendingPlan() (PlanProposed, bool) {
	l.plans.mu.Lock()
	defer l.plans.mu.Unlock()
	if l.plans.pending == nil {
		return PlanProposed{}, false
	}
	return *l.plans.pending, true
}

// TakePlan returns the plan the last run proposed, once.
func (l *Loop) TakePlan() (PlanProposed, bool) {
	l.plans.mu.Lock()
	defer l.plans.mu.Unlock()
	p := l.plans.pending
	l.plans.pending = nil
	if p == nil {
		return PlanProposed{}, false
	}
	return *p, true
}

// offered reports whether a tool is offered in the current mode.
func (l *Loop) offered(t tools.Tool) bool {
	scoped, ok := t.(tools.ModeScoped)
	return !ok || l.Policy != nil && string(l.Policy.Mode) == scoped.OnlyInMode()
}

// offeredDefs are the definitions of the tools offered in the current mode.
func (l *Loop) offeredDefs() []model.ToolDef {
	defs := toolDefs(l.Tools)
	out := defs[:0:0]
	for _, d := range defs {
		if t, ok := l.Tools.Get(d.Name); ok && !l.offered(t) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// offeredNames are the names of the tools offered in the current mode.
func (l *Loop) offeredNames() []string {
	var out []string
	for _, d := range l.offeredDefs() {
		out = append(out, d.Name)
	}
	return out
}
