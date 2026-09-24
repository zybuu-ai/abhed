package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// steeringAdapter calls a tool on the first turn, then answers. It reports what
// it saw so a test can prove the steering message arrived.
type steeringAdapter struct {
	loop     *Loop
	steerAt  int
	message  string
	turns    int
	lastSeen []model.Message
}

func (s *steeringAdapter) Name() string                           { return "steering" }
func (s *steeringAdapter) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (s *steeringAdapter) CountTokens(model.Request) (int, error) { return 0, nil }

func (s *steeringAdapter) Complete(ctx context.Context, req model.Request) (<-chan model.Chunk, error) {
	s.turns++
	s.lastSeen = req.Messages
	ch := make(chan model.Chunk, 4)

	// Steer while the agent is mid-run, the way a user would.
	if s.turns == s.steerAt && s.loop != nil {
		s.loop.Steer(s.message)
	}

	if s.turns < 3 {
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &model.ToolCall{
			ID: "c1", Name: "glob", Args: json.RawMessage(`{"pattern":"*.go"}`),
		}}
	} else {
		ch <- model.Chunk{Type: model.ChunkText, Text: "done"}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

func steerHarness(t *testing.T, ad model.Adapter) (*Loop, *MemStore) {
	t.Helper()
	dir := tempDir(t)
	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	rec := NewRecorder(store, "sess1", "")
	reg := tools.NewRegistry(tools.Read{}, tools.Glob{}, tools.Grep{})
	l := NewLoop(ad, reg, policy.New(policy.ModeAuto),
		AutoApprove{Yes: true}, sess, rec, DefaultConfig())
	return l, store
}

// A message sent while the agent works must reach the model, not be dropped and
// not require killing the run.
func TestSteeringReachesTheModel(t *testing.T) {
	ad := &steeringAdapter{steerAt: 1, message: "actually, only look at internal/"}
	loop, store := steerHarness(t, ad)
	ad.loop = loop

	if _, err := loop.Run(context.Background(), "list the go files"); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, m := range ad.lastSeen {
		if strings.Contains(m.Content, "only look at internal/") {
			found = true
		}
	}
	if !found {
		t.Fatal("the steering message never reached the model")
	}

	// And it is recorded, so a replay shows the user changed course.
	evs, _ := store.Events("sess1")
	users := 0
	for _, ev := range evs {
		if ev.Type == EvUserMessage {
			users++
		}
	}
	if users != 2 {
		t.Errorf("recorded %d user messages, want 2 (the prompt and the steer)", users)
	}
}

// Steering must not consume a turn from the budget: a correction is not the
// agent's work.
func TestSteeringDoesNotCostATurn(t *testing.T) {
	adPlain := &steeringAdapter{steerAt: 99} // never steers
	plain, _ := steerHarness(t, adPlain)
	if _, err := plain.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	baseline := plain.Usage().Turns

	adSteer := &steeringAdapter{steerAt: 1, message: "change of plan"}
	steered, _ := steerHarness(t, adSteer)
	adSteer.loop = steered
	if _, err := steered.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := steered.Usage().Turns; got != baseline {
		t.Errorf("turns = %d with steering, %d without; steering must not cost a turn", got, baseline)
	}
}

// Several messages sent in quick succession all arrive, in order.
func TestMultipleSteersArriveInOrder(t *testing.T) {
	ad := &steeringAdapter{steerAt: 99}
	loop, _ := steerHarness(t, ad)
	loop.Steer("first")
	loop.Steer("second")
	got := loop.takeSteering()
	if len(got) != 2 || got[0].Text != "first" || got[1].Text != "second" {
		t.Fatalf("takeSteering() = %v, want [first second]", got)
	}
	if again := loop.takeSteering(); again != nil {
		t.Errorf("messages must be delivered once, got %v on the second read", again)
	}
}

func TestEmptySteerIsIgnored(t *testing.T) {
	ad := &steeringAdapter{steerAt: 99}
	loop, _ := steerHarness(t, ad)
	loop.Steer("   \n ")
	if got := loop.takeSteering(); got != nil {
		t.Errorf("whitespace must not become a message, got %v", got)
	}
	if id := loop.Queue(" "); id != "" {
		t.Errorf("a blank message was given id %q", id)
	}
}

// A queued message carries its id to the user.message that delivers it, and
// one withdrawn before delivery never reaches the model or the record.
func TestQueuedMessagesAreMatchedAndCancellable(t *testing.T) {
	ad := &steeringAdapter{steerAt: 99}
	loop, store := steerHarness(t, ad)
	keep := loop.Queue("keep this")
	drop := loop.Queue("drop this")
	last := loop.Queue("and this")
	if keep == "" || keep == drop || drop == last {
		t.Fatalf("ids must be distinct and non-empty: %q %q %q", keep, drop, last)
	}
	if !loop.Unqueue(drop) {
		t.Fatal("a pending message could not be withdrawn")
	}
	if loop.Unqueue(drop) {
		t.Error("a message was withdrawn twice")
	}
	if q := loop.Queued(); len(q) != 2 || q[0].ID != keep || q[1].ID != last {
		t.Fatalf("Queued() = %v, want the two kept, oldest first", q)
	}

	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if loop.Unqueue(keep) {
		t.Error("a delivered message could still be withdrawn")
	}
	evs, _ := store.Events("sess1")
	var got []Message
	for _, ev := range evs {
		if ev.Type == EvUserMessage {
			var m Message
			_ = json.Unmarshal(ev.Payload, &m)
			got = append(got, m)
		}
	}
	// Left over from before the prompt, so they keep their place ahead of it.
	want := []Message{{Text: "keep this", QueueID: keep}, {Text: "and this", QueueID: last}, {Text: "go"}}
	if len(got) != len(want) {
		t.Fatalf("user messages = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("user message %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, m := range ad.lastSeen {
		if strings.Contains(m.Content, "drop this") {
			t.Fatal("a withdrawn message reached the model")
		}
	}
}

// finalTurnAdapter answers at once, and queues a message during that answer,
// after the loop's last look at the queue.
type finalTurnAdapter struct {
	loop  *Loop
	turns int
	seen  []model.Message
}

func (f *finalTurnAdapter) Name() string                           { return "final" }
func (f *finalTurnAdapter) Profile() model.Profile                 { return model.Profile{ContextWindow: 100000} }
func (f *finalTurnAdapter) CountTokens(model.Request) (int, error) { return 0, nil }
func (f *finalTurnAdapter) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	f.turns++
	f.seen = req.Messages
	if f.turns == 1 {
		f.loop.Queue("one more thing")
	}
	ch := make(chan model.Chunk, 2)
	ch <- model.Chunk{Type: model.ChunkText, Text: fmt.Sprintf("answer %d", f.turns)}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{}}
	close(ch)
	return ch, nil
}

// A message queued while the last turn was answering is read in the same run,
// not left waiting for a prompt that may never come.
func TestMessageQueuedDuringTheFinalTurnIsAnswered(t *testing.T) {
	ad := &finalTurnAdapter{}
	loop, store := steerHarness(t, ad)
	ad.loop = loop
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if ad.turns != 2 {
		t.Fatalf("model called %d times, want 2", ad.turns)
	}
	if last := ad.seen[len(ad.seen)-1]; last.Content != "one more thing" {
		t.Errorf("the second turn ended on %q, want the queued message", last.Content)
	}
	evs, _ := store.Events("sess1")
	ended := 0
	for _, ev := range evs {
		if ev.Type == EvSessionEnded {
			ended++
		}
	}
	if ended != 1 {
		t.Errorf("session.ended recorded %d times, want 1", ended)
	}
	if reason, err := loop.RunQueued(context.Background()); err != nil || reason != TermCompleted {
		t.Errorf("RunQueued with nothing queued = %s, %v", reason, err)
	}
	if again, _ := store.Events("sess1"); len(again) != len(evs) {
		t.Error("RunQueued with nothing queued wrote to the record")
	}
}
