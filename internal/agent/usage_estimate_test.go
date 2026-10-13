package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// stoppedMidReply streams part of a reply, then is stopped before any usage arrives.
type stoppedMidReply struct{ cancel context.CancelFunc }

func (s *stoppedMidReply) Name() string                           { return "stopped" }
func (s *stoppedMidReply) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (s *stoppedMidReply) CountTokens(model.Request) (int, error) { return 500, nil }
func (s *stoppedMidReply) Complete(ctx context.Context, _ model.Request) (<-chan model.Chunk, error) {
	ch := make(chan model.Chunk, 3)
	ch <- model.Chunk{Type: model.ChunkText, Text: "Let me look at the files in this folder first."}
	s.cancel()
	ch <- model.Chunk{Type: model.ChunkError, Err: ctx.Err()}
	close(ch)
	return ch, nil
}

// A stopped reply still cost tokens: they are estimated, marked so, and spent.
func TestStoppedReplyEstimatesItsTokens(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := tools.NewSession(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	l := NewLoop(&stoppedMidReply{cancel: cancel}, tools.NewRegistry(tools.Read{}), policy.New(policy.ModeAuto),
		AutoApprove{Yes: true}, sess, NewRecorder(store, "sess1", ""), DefaultConfig())
	l.Budget = NewBudget(1_000_000, 10, false)
	if _, err := l.Run(ctx, "go"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	var mc ModelCall
	for _, ev := range evs {
		if ev.Type == EvModelCall {
			_ = json.Unmarshal(ev.Payload, &mc)
		}
	}
	if !mc.Estimated || mc.TokensIn != 500 || mc.TokensOut == 0 {
		t.Fatalf("model.call = %+v, want estimated tokens", mc)
	}
	if got := l.Budget.tokens.Load(); got != int64(mc.TokensIn+mc.TokensOut) {
		t.Fatalf("budget spent %d, want %d", got, mc.TokensIn+mc.TokensOut)
	}
	// The session's totals, and so its session.ended, count them as HawkEYE's turns do.
	var ended struct {
		Reason    string `json:"reason"`
		TokensIn  int    `json:"tokens_in"`
		TokensOut int    `json:"tokens_out"`
	}
	for _, ev := range evs {
		if ev.Type == EvSessionEnded {
			_ = json.Unmarshal(ev.Payload, &ended)
		}
	}
	if u := l.Usage(); u.InputTokens != mc.TokensIn || u.OutputTokens != mc.TokensOut || ended.TokensIn != mc.TokensIn || ended.TokensOut != mc.TokensOut {
		t.Fatalf("totals %+v, session.ended %+v, want the call's %d in, %d out", u, ended, mc.TokensIn, mc.TokensOut)
	}
}

// After a compaction the next check leaves out the room for a turn, so a small
// window does not compact twice in a row.
func TestNoSecondCompactionRightAfterOne(t *testing.T) {
	ad := &summarizerAdapter{summary: "summary", window: 1000}
	l, _, _ := harness(t, nil, policy.ModeDefault, true)
	l.Compactor = NewCompactor(ad, 0.9)
	l.messages = longMessages(10, 280) // 700 tokens: over the line only with the turn's room added
	l.justCompacted = true
	if err := l.maybeCompact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.usage.Compactions != 0 {
		t.Fatal("compacted again right after a compaction")
	}
	if err := l.maybeCompact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.usage.Compactions != 1 {
		t.Fatal("the check after that one should keep the room for a turn")
	}
}
