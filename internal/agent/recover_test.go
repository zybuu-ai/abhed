package agent

import (
	"testing"
)

// crashedRecord is a session a process left mid-run with one background
// child running: spawned, never returned, no end.
func crashedRecord(t *testing.T) (*MemStore, []Event) {
	t.Helper()
	st := NewMemStore()
	p := NewRecorder(st, "p", "")
	c := NewRecorder(st, "c", "p")
	_, _ = p.Record(EvSessionStarted, ActorSystem, Trusted, map[string]string{})
	_, _ = p.Record(EvUserMessage, ActorUser, Trusted, Message{Text: "go"})
	_, _ = p.Record(EvSubagentSpawned, ActorAgent, Trusted, map[string]any{"session": "c", "task_id": "c", "background": true, "tokens_in": 0})
	_, _ = c.Record(EvSubagentSpawned, ActorAgent, Trusted, map[string]any{"session": "c", "task_id": "c", "background": true})
	_, _ = c.Record(EvUserMessage, ActorUser, Trusted, Message{Text: "child work"})
	_, _ = p.Record(EvModelCall, ActorSystem, Trusted, ModelCall{Turn: 1})
	evs, _ := st.Events("p")
	return st, evs
}

// A record a crashed process left open is reconciled once: the lost child is
// returned lost in the parent and ended recovered in its own record, and the
// parent gets a recovered end; a second pass finds nothing to do.
func TestReconcileOrphan(t *testing.T) {
	st, evs := crashedRecord(t)
	if !Orphaned(evs) {
		t.Fatal("a record with no end is not seen as orphaned")
	}
	if err := Reconcile(st, "p", evs); err != nil {
		t.Fatal(err)
	}
	evs, _ = st.Events("p")
	ret := payloads[map[string]any](evs, EvSubagentReturn)
	end, _ := LastEnd(evs)
	if len(ret) != 1 || ret[0]["reason"] != string(TermLost) || end.Reason != TermShutdown || !end.Recovered || end.Background != 0 {
		t.Fatalf("returned %v, end %+v", ret, end)
	}
	child, _ := st.Events("c")
	if cend, _ := LastEnd(child); cend.Reason != TermShutdown || !cend.Recovered {
		t.Fatalf("the child's end: %+v", cend)
	}
	if Orphaned(evs) {
		t.Fatal("a reconciled record is still orphaned")
	}
	n := len(evs)
	if err := Reconcile(st, "p", evs); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.Events("p"); len(again) != n {
		t.Fatal("reconciled twice")
	}
	// The lost child's result is owed to the conversation, as any other.
	if p := PendingNotices(evs, st.Events); len(p) != 1 || p[0].Reason != string(TermLost) {
		t.Fatalf("pending: %+v", p)
	}
}

// A settled record, and one whose last run ended with no child running, are
// not orphans.
func TestNotOrphaned(t *testing.T) {
	st := NewMemStore()
	p := NewRecorder(st, "p", "")
	_, _ = p.Record(EvUserMessage, ActorUser, Trusted, Message{Text: "go"})
	_, _ = p.Record(EvSessionEnded, ActorSystem, Trusted, SessionEnded{Reason: TermCompleted})
	evs, _ := st.Events("p")
	if Orphaned(evs) {
		t.Fatal("a finished record is an orphan")
	}
	_, _ = p.Record(EvSessionEnded, ActorSystem, Trusted, SessionEnded{Reason: TermCompleted, Background: 1})
	evs, _ = st.Events("p")
	if !Orphaned(evs) {
		t.Fatal("an end with children running, never settled, is not an orphan")
	}
}

// The spend a record holds is carried: the parent's and every child's
// tokens, and each spawn; Carry only raises the counters.
func TestCarriedSpend(t *testing.T) {
	st := NewMemStore()
	p := NewRecorder(st, "p", "")
	_, _ = p.Record(EvSubagentSpawned, ActorAgent, Trusted, map[string]any{"session": "a"})
	_, _ = p.Record(EvSubagentReturn, ActorAgent, Trusted, map[string]any{"session": "a", "tokens_in": 100, "tokens_out": 20})
	_, _ = p.Record(EvSubagentSpawned, ActorAgent, Trusted, map[string]any{"session": "b"})
	_, _ = p.Record(EvSessionEnded, ActorSystem, Trusted, SessionEnded{Reason: TermCompleted, TokensIn: 300, TokensOut: 30})
	evs, _ := st.Events("p")
	tokens, spawned := CarriedSpend(evs)
	if tokens != 450 || spawned != 2 {
		t.Fatalf("carried %d tokens, %d spawns", tokens, spawned)
	}
	b := NewBudget(500, 3, false)
	b.Carry(tokens, spawned)
	if b.Spent() != 450 || b.spawned.Load() != 2 {
		t.Fatalf("budget %d %d", b.Spent(), b.spawned.Load())
	}
	b.Carry(10, 0)
	if b.Spent() != 450 {
		t.Fatal("Carry lowered the counter")
	}
	if err := b.TryReserveSubagent(); err != nil {
		t.Fatal(err)
	}
	if err := b.TryReserveSubagent(); err == nil {
		t.Fatal("a continued session got a fresh spawn allowance")
	}
}
