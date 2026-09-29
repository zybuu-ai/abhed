package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
)

// bothAsk is a model whose root turn asks for a command and starts a
// subagent in the same turn; the subagent asks for a command of its own.
type bothAsk struct{}

func (bothAsk) Name() string                           { return "both" }
func (bothAsk) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (bothAsk) CountTokens(model.Request) (int, error) { return 0, nil }

func (bothAsk) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 4)
	last := req.Messages[len(req.Messages)-1]
	switch {
	case last.Role == model.RoleTool:
		ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	case last.Content == "go":
		own := bashCall("own", "touch parent.txt")
		task := model.ToolCall{ID: "t", Name: "task", Args: json.RawMessage(`{"prompt":"child","description":"child"}`)}
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &own}
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &task}
	default:
		c := bashCall("b", "touch child.txt")
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &c}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

// A parent's own ask and its subagent's ask, made at the same time, reach
// the approver one after the other: the parent's asks take the tree's one
// queue too, so a person is never put two questions at once. Within one run
// the loop settles a turn's asks before any child starts; a child that runs
// beside its parent's own turns (in the background) meets this.
func TestParentAndChildAsksNeverOverlap(t *testing.T) {
	for range 3 {
		appr := &countingApprover{}
		store := NewMemStore()
		l, _, f := taskTree(t, bothAsk{}, appr, store, store, false)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		child := l.asParent(ctx) // what the context of a task call carries
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			own := bashCall("own", "touch parent.txt")
			l.authorize(ctx, &own)
		}()
		go func() {
			defer wg.Done()
			_, _ = f.Spawn(child, SubagentRequest{Prompt: "child", Description: "child"})
		}()
		wg.Wait()
		cancel()
		if appr.asked.Load() != 2 || appr.peak.Load() != 1 {
			t.Fatalf("want 2 asks one at a time, got %d asks and %d at once", appr.asked.Load(), appr.peak.Load())
		}
	}
}

// A loop's own ask waiting in the queue gives up without asking when its
// run is cancelled.
func TestOwnAskGivesUpInTheQueue(t *testing.T) {
	appr := &countingApprover{}
	l, _ := harnessIn(t, tempDir(t), []scriptedTurn{{calls: []model.ToolCall{bashCall("b", "touch x")}}}, "default", false)
	l.Approver = appr
	queue := l.askQueue(context.Background())
	queue <- struct{}{} // another ask holds the queue
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _ = l.Run(ctx, "go")
	if appr.asked.Load() != 0 {
		t.Fatal("an ask was put while another held the queue")
	}
}
