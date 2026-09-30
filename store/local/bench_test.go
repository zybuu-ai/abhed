package local

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

func benchStore(b *testing.B) *agent.Recorder {
	s, err := Open(Options{Dir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	if err := s.CreateSession(context.Background(), store.SessionRecord{ID: "s-bench"}); err != nil {
		b.Fatal(err)
	}
	return agent.NewRecorder(s, "s-bench", "")
}

// BenchmarkAppendStreamed is an append with no sync: a streamed fragment.
func BenchmarkAppendStreamed(b *testing.B) {
	rec := benchStore(b)
	for b.Loop() {
		if _, err := rec.Record(agent.EvAgentDelta, agent.ActorAgent, agent.Trusted, agent.Delta{Text: "a few words of a reply"}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAppendSynced is an append that syncs and moves the head.
func BenchmarkAppendSynced(b *testing.B) {
	rec := benchStore(b)
	for b.Loop() {
		if _, err := rec.Record(agent.EvModelCall, agent.ActorSystem, agent.Trusted, agent.ModelCall{Turn: 1, TokensIn: 1200}); err != nil {
			b.Fatal(err)
		}
	}
}

// TestBudgets holds the plan's budgets: an unsynced append within 1 ms, a
// synced one within 15 ms, and verifying 100,000 events within 2 s. They
// are measured, not assumed, and skipped under -short and the race detector.
func TestBudgets(t *testing.T) {
	if testing.Short() || raceEnabled || os.Getenv("ABHED_SKIP_BUDGETS") != "" {
		t.Skip("budgets are measured without -short and -race")
	}
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.CreateSession(context.Background(), store.SessionRecord{ID: "s-budget"})
	rec := agent.NewRecorder(s, "s-budget", "")
	const n = 2000
	start := time.Now()
	for range n {
		_, _ = rec.Record(agent.EvAgentDelta, agent.ActorAgent, agent.Trusted, agent.Delta{Text: "words"})
	}
	if per := time.Since(start) / n; per > time.Millisecond {
		t.Errorf("unsynced append took %v, budget 1ms", per)
	}
	start = time.Now()
	for range 50 {
		_, _ = rec.Record(agent.EvModelCall, agent.ActorSystem, agent.Trusted, agent.ModelCall{Turn: 1})
	}
	if per := time.Since(start) / 50; per > 15*time.Millisecond {
		t.Errorf("synced append took %v, budget 15ms", per)
	}
	// 100k events, written straight to the file, then verified.
	for range 100_000 - n - 50 {
		_, _ = rec.Record(agent.EvAgentDelta, agent.ActorAgent, agent.Trusted, agent.Delta{Text: "words"})
	}
	_ = s.Sync("s-budget")
	start = time.Now()
	rep, err := s.Verify("s-budget")
	if err != nil || !rep.OK || rep.Events != 100_000 {
		t.Fatalf("verify: %+v %v", rep, err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("verifying 100k events took %v, budget 2s", took)
	}
}
