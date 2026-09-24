package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// cancellingAdapter cancels the run mid-stream, the way Stop does, and then
// reports the cut stream as an error, as a real adapter does.
type cancellingAdapter struct{ cancel context.CancelFunc }

func (c *cancellingAdapter) Name() string                           { return "cancelling" }
func (c *cancellingAdapter) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (c *cancellingAdapter) CountTokens(model.Request) (int, error) { return 0, nil }
func (c *cancellingAdapter) Complete(ctx context.Context, _ model.Request) (<-chan model.Chunk, error) {
	c.cancel()
	ch := make(chan model.Chunk, 2)
	ch <- model.Chunk{Type: model.ChunkError, Err: ctx.Err()}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

// A stream cut by our own interrupt is recorded as an interrupt, not as a
// model error that a reader would take for a failure.
func TestInterruptIsNotAModelError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := tools.NewSession(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	l := NewLoop(&cancellingAdapter{cancel: cancel}, tools.NewRegistry(tools.Read{}), policy.New(policy.ModeAuto),
		AutoApprove{Yes: true}, sess, NewRecorder(store, "sess1", ""), DefaultConfig())
	reason, err := l.Run(ctx, "go")
	if err != nil || reason != TermUserInterrupt {
		t.Fatalf("Run = %s, %v; want user_interrupt", reason, err)
	}
	evs, _ := store.Events("sess1")
	calls := 0
	for _, ev := range evs {
		if ev.Type == EvModelCall {
			calls++
			var mc ModelCall
			_ = json.Unmarshal(ev.Payload, &mc)
			if mc.Error != "" {
				t.Fatalf("model.call recorded %q for an interrupt", mc.Error)
			}
		}
	}
	if calls != 1 {
		t.Fatalf("recorded %d model.call events, want 1", calls)
	}
}
