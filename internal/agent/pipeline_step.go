package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// ErrNoLoop refuses a pipeline that has no running loop to put its steps through.
var ErrNoLoop = errors.New("no session is running to check and record the pipeline's steps, so it was not run")

type viaKey struct{}

type approverKey struct{}

// viaOf names what issued a call on the agent's behalf, such as a skill's pipeline.
func viaOf(ctx context.Context) string { v, _ := ctx.Value(viaKey{}).(string); return v }

// approverFor is the approver a call is put to: one a pipeline step carries, the loop's, or a refusal.
func (l *Loop) approverFor(ctx context.Context) Approver {
	if a, ok := ctx.Value(approverKey{}).(Approver); ok && a != nil {
		return a
	}
	if l.Approver == nil {
		return AutoApprove{Yes: false}
	}
	return l.Approver
}

// Steps runs one pipeline's tool steps through the loop, as the model's own calls are run.
type Steps struct {
	loop *Loop
	via  string
	auth sync.Mutex   // one authorization at a time, so asks never race
	run  sync.RWMutex // a mutating step runs alone, as in a turn
}

// Steps returns the runner for one pipeline, or ErrNoLoop before a loop is set.
func (h *LoopHolder) Steps(via string) (*Steps, error) {
	if h == nil || h.loop == nil {
		return nil, ErrNoLoop
	}
	return &Steps{loop: h.loop, via: via}, nil
}

// Run authorizes and runs one tool step. A refusal comes back as an error
// result, recorded like the model's; an error means the step could not be judged.
func (s *Steps) Run(ctx context.Context, name string, args json.RawMessage) (tools.Result, error) {
	l := s.loop
	call := model.ToolCall{ID: "step_" + newID(), Name: name, Args: args}
	// Only the judgement carries these; a subagent the step spawns keeps its own.
	actx := context.WithValue(context.WithValue(ctx, viaKey{}, s.via), approverKey{}, s.approver(ctx))

	s.auth.Lock()
	ok, res, term := l.authorize(actx, call)
	s.auth.Unlock()
	if term != "" {
		return res, fmt.Errorf("%s: %s", term, res.Content)
	}
	if !ok {
		return res, nil
	}

	tool, _ := l.Tools.Get(name)
	if tool != nil && tool.Mutates() {
		s.run.Lock()
		defer s.run.Unlock()
	} else {
		s.run.RLock()
		defer s.run.RUnlock()
	}
	res, term = l.invoke(ctx, call)
	if term != "" {
		return res, fmt.Errorf("%s: %s", term, res.Content)
	}
	return res, nil
}

// Redact strips secret values from text bound for a pipeline's model step.
func (s *Steps) Redact(text string) string {
	if red := s.loop.Recorder.redactor(); red != nil {
		return redactedText(red.Redact, text)
	}
	return text
}

// approver shares the asks queue of the loop's tree, as a subagent's does.
func (s *Steps) approver(ctx context.Context) Approver {
	l := s.loop
	l.asksOnce.Do(func() { l.asks = make(chan struct{}, 1) })
	asks := l.asks
	if p, ok := ctx.Value(parentKey{}).(*parentLink); ok && p.asks != nil {
		asks = p.asks
	}
	inner := l.approverFor(context.Background())
	if o, nested := inner.(oneAtATime); nested {
		inner = o.Approver
	}
	return oneAtATime{Approver: inner, asks: asks, who: s.via}
}
