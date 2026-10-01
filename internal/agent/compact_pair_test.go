package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A history whose only older message is the previous summary.
func summaryThenFourExchanges() []model.Message {
	msgs := []model.Message{{Role: model.RoleUser, Content: "[Earlier conversation was compacted] summary"}}
	for _, w := range []string{"one", "two", "three", "four"} {
		msgs = append(msgs,
			model.Message{Role: model.RoleUser, Content: "ask " + w},
			model.Message{Role: model.RoleAssistant, Content: "answer " + w})
	}
	return msgs
}

func pairLoop(t *testing.T) (*Loop, *MemStore, *int) {
	t.Helper()
	sess, err := tools.NewSession(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	adapter := &sizedAdapter{scriptedAdapter: &scriptedAdapter{}, window: 1000}
	l := NewLoop(adapter, tools.NewRegistry(tools.Read{}), policy.New(policy.ModeDefault),
		AutoApprove{Yes: true}, sess, NewRecorder(store, "pair", ""), DefaultConfig())
	l.Compactor = NewCompactor(adapter, 0.01)
	calls := 0
	l.Compactor.Summarizer = func([]model.Message) (string, bool) { calls++; return "S", false }
	return l, store, &calls
}

func compactionEvents(t *testing.T, store *MemStore) (started, done int) {
	t.Helper()
	evs, _ := store.Events("pair")
	for _, e := range evs {
		switch e.Type {
		case EvCompactStarted:
			started++
		case EvCompactDone:
			done++
		}
	}
	return started, done
}

// Re-summarising the previous summary alone changes nothing; it once recorded
// a started with no completion and paid for the summary it then discarded.
func TestAutoCompactionSameLengthRecordsNothing(t *testing.T) {
	l, store, calls := pairLoop(t)
	l.messages = summaryThenFourExchanges()
	if err := l.compactNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, d := compactionEvents(t, store); s != 0 || d != 0 {
		t.Fatalf("started %d, completed %d; want neither", s, d)
	}
	if *calls != 0 {
		t.Fatalf("summariser called %d times for nothing to summarise", *calls)
	}
}

func TestManualCompactNothingToSummariseRecordsNothing(t *testing.T) {
	for name, msgs := range map[string][]model.Message{
		"empty":       nil,
		"same length": summaryThenFourExchanges(),
	} {
		t.Run(name, func(t *testing.T) {
			l, store, _ := pairLoop(t)
			l.messages = msgs
			if _, err := l.Compact(context.Background()); !errors.Is(err, ErrNothingToCompact) {
				t.Fatalf("err = %v, want ErrNothingToCompact", err)
			}
			if s, d := compactionEvents(t, store); s != 0 || d != 0 {
				t.Fatalf("started %d, completed %d; want neither", s, d)
			}
			if l.usage.Compactions != 0 {
				t.Fatalf("counted %d compactions", l.usage.Compactions)
			}
		})
	}
}

// With two older messages there is something to summarise: one started, one
// completion.
func TestAutoCompactionPairsStartedWithCompleted(t *testing.T) {
	l, store, calls := pairLoop(t)
	l.messages = append([]model.Message{{Role: model.RoleUser, Content: "first"},
		{Role: model.RoleAssistant, Content: "reply"}}, summaryThenFourExchanges()[1:]...)
	if err := l.compactNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, d := compactionEvents(t, store); s != 1 || d != 1 || *calls != 1 {
		t.Fatalf("started %d, completed %d, summaries %d; want 1 each", s, d, *calls)
	}
}
