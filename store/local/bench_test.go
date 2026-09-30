package local

import (
	"context"
	"os"
	"slices"
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
// synced one within 15 ms, and verifying 100,000 events within 2 s. Each is
// the median of several runs, so one slow moment on a busy machine does not
// fail it; they are skipped under -short and the race detector.
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
	median := func(n int, f func()) time.Duration {
		d := make([]time.Duration, n)
		for i := range d {
			start := time.Now()
			f()
			d[i] = time.Since(start)
		}
		slices.Sort(d)
		return d[n/2]
	}
	if m := median(2000, func() {
		_, _ = rec.Record(agent.EvAgentDelta, agent.ActorAgent, agent.Trusted, agent.Delta{Text: "words"})
	}); m > time.Millisecond {
		t.Errorf("unsynced append took %v (median), budget 1ms", m)
	}
	if m := median(51, func() {
		_, _ = rec.Record(agent.EvModelCall, agent.ActorSystem, agent.Trusted, agent.ModelCall{Turn: 1})
	}); m > 15*time.Millisecond {
		t.Errorf("synced append took %v (median), budget 15ms", m)
	}
	for range 100_000 - 2000 - 51 {
		_, _ = rec.Record(agent.EvAgentDelta, agent.ActorAgent, agent.Trusted, agent.Delta{Text: "words"})
	}
	_ = s.Sync("s-budget")
	if m := median(3, func() {
		rep, err := s.Verify("s-budget")
		if err != nil || !rep.OK || rep.Events != 100_000 {
			t.Fatalf("verify: %+v %v", rep, err)
		}
	}); m > 2*time.Second {
		t.Errorf("verifying 100k events took %v (median), budget 2s", m)
	}
}
