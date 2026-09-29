package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// ErrNoLoop refuses a pipeline started outside a loop's tool call, where no policy or record applies.
var ErrNoLoop = errors.New("a pipeline runs only from a session's own skill call, where its steps are checked and recorded; it was not run")

// ErrNestedPipeline refuses a pipeline started beneath another pipeline's step, which would have no bound.
var ErrNestedPipeline = errors.New("a pipeline cannot start beneath another pipeline's step, so it was not run")

type viaKey struct{}

type approverKey struct{}

type inPipelineKey struct{}

type pipelineAskKey struct{}

// viaOf names what issued a call on the agent's behalf, such as a skill's pipeline.
func viaOf(ctx context.Context) string { v, _ := ctx.Value(viaKey{}).(string); return v }

// PipelineOf is the pipeline an ask comes from, or "" when it is not a pipeline step.
func PipelineOf(ctx context.Context) string { v, _ := ctx.Value(pipelineAskKey{}).(string); return v }

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

// Steps runs one pipeline's tool steps on the loop whose skill call started it,
// as that loop runs the model's calls: its policy, session, depth and record.
type Steps struct {
	loop    *Loop
	via     string
	asks    chan struct{}
	adapter model.Adapter // the calling loop's model when the skill was called
	auth    sync.Mutex    // this pipeline's steps are judged one at a time
}

// StepsFor returns the runner for a pipeline started by the tool call ctx
// belongs to, or refuses one with no such call or beneath another pipeline.
func StepsFor(ctx context.Context, via string) (*Steps, error) {
	if ctx.Value(inPipelineKey{}) != nil {
		return nil, ErrNestedPipeline
	}
	p, _ := ctx.Value(parentKey{}).(*parentLink)
	if p == nil || p.loop == nil {
		return nil, ErrNoLoop
	}
	return &Steps{loop: p.loop, via: via, asks: p.asks, adapter: p.adapter}, nil
}

// Adapter is the model the calling loop runs on, which a switch may have changed.
func (s *Steps) Adapter() model.Adapter {
	if s.adapter != nil {
		return s.adapter
	}
	return s.loop.Adapter
}

// Input is the request the calling loop is answering: a pipeline needs it, and
// the model calls a skill by name without repeating the question.
func (s *Steps) Input() string { return s.loop.Prompt() }

// Run authorizes and runs one tool step. A refusal comes back as an error
// result, recorded like the model's; an error means the step could not be judged.
// runFor bounds the tool's run, not the wait for a person's answer; 0 is unbounded.
func (s *Steps) Run(ctx context.Context, name string, args json.RawMessage, runFor time.Duration) (tools.Result, error) {
	l := s.loop
	call := model.ToolCall{ID: "step_" + newID(), Name: name, Args: args}
	// Only the judgement carries these; a subagent the step spawns keeps its own.
	actx := context.WithValue(context.WithValue(ctx, viaKey{}, s.via), approverKey{}, s.approver())

	s.auth.Lock()
	ok, res, term := l.authorize(actx, &call)
	s.auth.Unlock()
	if term != "" {
		return res, s.ended(term, res)
	}
	if !ok {
		return res, nil
	}

	// A mutating step runs apart from every other pipeline step on this loop;
	// the turn's own read-only calls may still overlap it.
	tool, _ := l.Tools.Get(name)
	if tool != nil && tools.MutatesCall(tool, call.Args) {
		l.stepRun.Lock()
		defer l.stepRun.Unlock()
	} else {
		l.stepRun.RLock()
		defer l.stepRun.RUnlock()
	}
	ictx := context.WithValue(ctx, inPipelineKey{}, s.via)
	if runFor > 0 {
		var cancel context.CancelFunc
		ictx, cancel = context.WithTimeout(ictx, runFor)
		defer cancel()
	}
	res, term = l.invoke(ictx, call)
	if term != "" {
		return res, s.ended(term, res)
	}
	return res, nil
}

// ended reports a step that ended its call; an error ends the loop's run at
// its next turn, as the same failure on a model's call ends that turn.
func (s *Steps) ended(term TerminalReason, res tools.Result) error {
	err := fmt.Errorf("%s: %s", term, res.Content)
	if term == TermError {
		s.loop.noteRecordErr(err)
	}
	return err
}

// Redact strips secret values from text bound for a pipeline's model step.
func (s *Steps) Redact(text string) string {
	if red := s.loop.Recorder.redactor(); red != nil {
		return redactedText(red.Redact, text)
	}
	return text
}

// Stage records what the pipeline did at one stage, in the calling loop's record.
func (s *Steps) Stage(skill, stage, detail string, data map[string]any) {
	s.loop.RecordPipelineStage(skill, stage, detail, data)
}

// approver puts a step's asks on the tree's queue, labelled with the pipeline
// and, in a subagent, with that subagent too.
func (s *Steps) approver() Approver {
	inner, who := s.loop.approverFor(context.Background()), ""
	if o, nested := inner.(oneAtATime); nested {
		inner, who = o.Approver, o.who
	}
	asks := s.asks
	if asks == nil {
		asks = make(chan struct{}, 1)
	}
	return oneAtATime{Approver: inner, asks: asks, who: who, via: s.via}
}
