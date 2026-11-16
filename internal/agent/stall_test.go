package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
)

// thinker answers each call by its script: "runaway" reasons until the call
// is cut, "think-then-answer" reasons briefly then answers, "fail" fails
// after acceptance, anything else answers with that text.
type thinker struct {
	script []string
	reqs   []model.Request
}

func (t *thinker) Name() string { return "thinker" }
func (t *thinker) Profile() model.Profile {
	return model.Profile{Name: "gpt-4o", ContextWindow: 100000, Sampling: model.Sampling{Think: true}}
}
func (t *thinker) CountTokens(model.Request) (int, error) { return 100, nil }
func (t *thinker) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	step := "done"
	if n := len(t.reqs); n < len(t.script) {
		step = t.script[n]
	}
	t.reqs = append(t.reqs, req)
	ch := make(chan model.Chunk, 4)
	go func() {
		defer close(ch)
		switch step {
		case "runaway":
			for {
				select {
				case <-ctx.Done():
					ch <- model.Chunk{Type: model.ChunkError, Err: ctx.Err()}
					return
				case ch <- model.Chunk{Type: model.ChunkReasoning, Text: "I will call read_file(path='x')... "}:
					time.Sleep(5 * time.Millisecond)
				}
			}
		case "think-then-answer":
			ch <- model.Chunk{Type: model.ChunkReasoning, Text: "brief thought"}
			time.Sleep(20 * time.Millisecond)
			ch <- model.Chunk{Type: model.ChunkText, Text: "answer"}
			ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 100, OutputTokens: 5}}
		case "fail":
			ch <- model.Chunk{Type: model.ChunkError, Err: errors.New("stream reset")}
		default:
			ch <- model.Chunk{Type: model.ChunkText, Text: step}
			ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 100, OutputTokens: 5}}
		}
	}()
	return ch, nil
}

func stallLoop(t *testing.T, script ...string) (*Loop, *MemStore, *thinker) {
	t.Helper()
	l, store := costHarness(t, "gpt-4o", nil)
	th := &thinker{script: script}
	l.Adapter = th
	l.Config.ReasoningStall = 60 * time.Millisecond
	return l, store, th
}

// A reply that only reasons is cut at the bound, not held to the call's cap;
// the turn asks once more, with thinking off and a nudge, and completes.
func TestRunawayReasoningIsCutAndRetriedOnce(t *testing.T) {
	l, store, th := stallLoop(t, "runaway", "done")
	started := time.Now()
	reason, err := l.Run(context.Background(), "go")
	if err != nil || reason != TermCompleted || len(th.reqs) != 2 {
		t.Fatalf("ended %s %v after %d calls", reason, err, len(th.reqs))
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("took %s", took)
	}
	calls := modelCalls(t, store)
	if !strings.Contains(calls[0].Error, "reasoned for 60ms without answering") || !calls[0].Estimated || calls[0].TokensIn != 100 {
		t.Fatalf("the cut call %+v", calls[0])
	}
	retry := th.reqs[1]
	if retry.Params.Think == nil || *retry.Params.Think {
		t.Fatalf("the retry asked with thinking %v", retry.Params.Think)
	}
	last := retry.Messages[len(retry.Messages)-1]
	if last.Role != model.RoleUser || !strings.Contains(last.Content, "reasoned for 60ms without answering") {
		t.Fatalf("the retry's nudge %+v", last)
	}
}

// Two cut replies in a row end the run stalled, saying why; the reasoning is
// in the record.
func TestRunawayReasoningTwiceEndsStalled(t *testing.T) {
	l, store, th := stallLoop(t, "runaway", "runaway", "done")
	reason, _ := l.Run(context.Background(), "go")
	if reason != TermStalled || len(th.reqs) != 2 {
		t.Fatalf("ended %s after %d calls", reason, len(th.reqs))
	}
	evs := sessEvents(t, store)
	end, _ := LastEnd(evs)
	if !strings.Contains(end.Detail, "twice in a row") {
		t.Fatalf("detail %q", end.Detail)
	}
	if !hasEvent(evs, EvAgentReasoning) {
		t.Fatal("the reasoning was not kept")
	}
}

// Reasoning followed by an answer within the bound is not cut.
func TestReasoningThenAnswerIsNotCut(t *testing.T) {
	l, store, th := stallLoop(t, "think-then-answer")
	l.Config.ReasoningStall = 2 * time.Second
	if reason, _ := l.Run(context.Background(), "go"); reason != TermCompleted || len(th.reqs) != 1 {
		t.Fatalf("ended %s after %d calls", reason, len(th.reqs))
	}
	if c := modelCalls(t, store); c[0].Error != "" || c[0].Estimated {
		t.Fatalf("call %+v", c[0])
	}
}

// A cut reply is one of the three failed replies in a row that end a run.
func TestRunawayReasoningCountsTowardThreeFailures(t *testing.T) {
	l, _, th := stallLoop(t, "fail", "runaway", "fail", "done")
	if reason, _ := l.Run(context.Background(), "go"); reason != TermRetryExhausted || len(th.reqs) != 3 {
		t.Fatalf("ended %s after %d calls", reason, len(th.reqs))
	}
}

// Under a dollar cap a cut reply is charged at its output bound.
func TestRunawayReasoningIsChargedAtItsBoundUnderACap(t *testing.T) {
	l, store, _ := stallLoop(t, "runaway", "done")
	l.Budget.MaxUSD = 100
	l.Config.MaxTokens = 2048
	_, _ = l.Run(context.Background(), "go")
	if c := modelCalls(t, store); c[0].TokensOut != 2048 || c[0].CostUSD == nil {
		t.Fatalf("cut call %+v", c[0])
	}
}
