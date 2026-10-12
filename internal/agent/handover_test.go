package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// handOverRecord runs a turn that writes a.txt and b.txt, each asked, until
// b.txt's ask: there the run is handed over. It returns the record.
func handOverRecord(t *testing.T, dir string) []Event {
	t.Helper()
	turns := []scriptedTurn{{calls: []model.ToolCall{
		{ID: "cA", Name: "write", Args: json.RawMessage(`{"path":"` + filepath.Join(dir, "a.txt") + `","content":"a"}`)},
		{ID: "cB", Name: "write", Args: json.RawMessage(`{"path":"` + filepath.Join(dir, "b.txt") + `","content":"b"}`)},
	}}}
	l, store := harnessIn(t, dir, turns, policy.ModeDefault, false)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	asked := 0
	l.Approver = approverFunc(func(actx context.Context, _ policy.Result) (bool, error) {
		asked++
		if asked == 1 {
			return true, nil
		}
		l.Queue("a message the run never took")
		cancel(ErrHandOver)
		<-actx.Done()
		return false, actx.Err()
	})
	reason, err := l.Run(ctx, "write both")
	if err != nil || reason != TermHandedOver {
		t.Fatalf("Run = %s, %v; want handed_over", reason, err)
	}
	evs, _ := store.Events("sess1")
	return evs
}

// A run handed over leaves the ask it was waiting on open and records no end,
// so the process taking it over gives the request its outcome.
func TestHandOverLeavesTheTurnOpen(t *testing.T) {
	evs := handOverRecord(t, tempDir(t))
	if _, ended := LastEnd(evs); ended {
		t.Fatalf("a handed-over run recorded an end: %s", types(evs))
	}
	for _, e := range evs {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if (e.Type == EvActionApproved || e.Type == EvActionDenied) && p["call_id"] == "cB" {
			t.Fatalf("the open ask got an outcome: %s %v", e.Type, p)
		}
	}
	if n := countOf(evs, EvMessageDropped); n != 1 {
		t.Fatalf("the queued message was not recorded as dropped: %s", types(evs))
	}
	open, ok := InFlight(evs)
	if !ok || len(open.Calls) != 1 || open.Calls[0].Call.ID != "cB" || open.Calls[0].Approved {
		t.Fatalf("InFlight = %+v %v", open, ok)
	}
}

func countOf(evs []Event, typ EventType) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// crashedAt is the record as a process that died just after step seq left it.
func crashedAt(t *testing.T, evs []Event, seq int64) *MemStore {
	t.Helper()
	st := NewMemStore()
	for _, e := range evs {
		if e.Seq <= seq {
			if err := st.Append(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	return st
}

// Another process takes the turn over: the call that may have run is not run
// again and is answered as outcome unknown; the one that never ran is asked
// again as a new call; every request has one outcome; the model goes on; and
// the same call as the unknown one later asks a person, whatever the mode.
func TestContinueTurnAsksAgainAndNeverReRunsUnknown(t *testing.T) {
	dir := tempDir(t)
	evs := handOverRecord(t, dir)
	// The process died just after asking about cB: cA was approved, its result never recorded.
	var cutSeq int64
	for _, e := range evs {
		var a ActionRequested
		if e.Type == EvActionRequested && json.Unmarshal(e.Payload, &a) == nil && a.CallID == "cB" {
			cutSeq = e.Seq
		}
	}
	st := crashedAt(t, evs, cutSeq)
	evs, _ = st.Events("sess1")
	open, ok := InFlight(evs)
	if !ok || len(open.Calls) != 2 || !open.Calls[0].Approved || open.Calls[1].Approved {
		t.Fatalf("InFlight = %+v %v", open, ok)
	}

	sess, err := tools.NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	again := model.ToolCall{ID: "cA2", Name: "write", Args: json.RawMessage(`{"path":"` + filepath.Join(dir, "a.txt") + `","content":"a"}`)}
	ad := &scriptedAdapter{turns: []scriptedTurn{{calls: []model.ToolCall{again}}, {text: "finished"}}}
	rec := NewRecorder(st, "sess1", "")
	rec.Advance(evs[len(evs)-1].Seq)
	reg := tools.NewRegistry(tools.Read{}, tools.Write{})
	var steps []string
	l := NewLoop(ad, reg, policy.New(policy.ModeAcceptEdits), approverFunc(func(_ context.Context, res policy.Result) (bool, error) {
		steps = append(steps, res.Step)
		return false, nil
	}), sess, rec, DefaultConfig())
	msgs, err := Fork(evs, 0)
	if err != nil {
		t.Fatal(err)
	}
	l.SetHistory(msgs, 1)
	reason, err := l.ContinueTurn(context.Background(), evs, open)
	if err != nil || reason != TermCompleted {
		t.Fatalf("ContinueTurn = %s, %v", reason, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the call whose outcome is unknown ran: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "b.txt")); err != nil || string(b) != "b" {
		t.Fatalf("the call asked again did not run: %q %v", b, err)
	}
	// The same write as cA asked a person, in a mode that allows writes, and was refused.
	if !slices.Equal(steps, []string{"handover"}) {
		t.Fatalf("asks = %v; want one, for the repeated call", steps)
	}

	evs, _ = st.Events("sess1")
	if n := countOf(evs, EvTurnContinued); n != 1 {
		t.Fatalf("turn.continued recorded %d times", n)
	}
	// Every request has exactly one outcome or result, all before the end.
	requested := map[string]bool{}
	outcomes := map[string]int{}
	ended := false
	for _, e := range evs {
		var p struct {
			CallID         string `json:"call_id"`
			OutcomeUnknown bool   `json:"outcome_unknown"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		switch e.Type {
		case EvSessionEnded:
			ended = true
		case EvActionRequested:
			requested[p.CallID] = true
		case EvActionApproved, EvActionDenied:
			if ended {
				t.Fatalf("an outcome for %s after the end", p.CallID)
			}
			outcomes[p.CallID]++
		case EvObservation:
			if p.CallID == "cA" && !p.OutcomeUnknown {
				t.Fatalf("cA answered without outcome_unknown")
			}
		}
	}
	for id := range requested {
		if outcomes[id] != 1 {
			t.Errorf("request %s has %d outcomes", id, outcomes[id])
		}
	}
	if !requested["cB_again2"] {
		t.Fatalf("cB was not asked again as cB_again2: %v", requested)
	}
	if !ended {
		t.Fatal("the continued run recorded no end")
	}
	// A rebuild of the record gives the conversation the run had.
	rebuilt, err := Fork(evs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shape(rebuilt), shape(l.Messages()); !slices.Equal(got, want) {
		t.Fatalf("rebuilt %v\nlive    %v", got, want)
	}
}

// shape is a conversation's roles, calls and results by id.
func shape(msgs []model.Message) []string {
	var out []string
	for _, m := range msgs {
		s := string(m.Role)
		for _, c := range m.ToolCalls {
			s += " " + c.ID
		}
		if m.ToolCallID != "" {
			s += " " + m.ToolCallID
		}
		out = append(out, s)
	}
	return out
}

// A record that is continued later anywhere keeps the ask on the call whose
// outcome it answered as unknown.
func TestUnknownOutcomeIsArmedFromTheRecord(t *testing.T) {
	l, _, _ := harness(t, nil, policy.ModeAcceptEdits, true)
	args := json.RawMessage(`{"path":"x","content":"y"}`)
	l.NoteUnknownOutcomes([]Event{
		{Seq: 1, Type: EvActionRequested, Payload: mustJSON(ActionRequested{CallID: "c1", Tool: "write", Args: args})},
		{Seq: 2, Type: EvObservation, Payload: mustJSON(Observation{CallID: "c1", Tool: "write", OutcomeUnknown: true})},
	})
	if !l.unknown.has("write", args) || l.unknown.has("write", json.RawMessage(`{}`)) {
		t.Fatal("the unknown outcome was not armed from the record")
	}
}

// A turn whose answer was recorded but not its end is ended, not answered twice.
func TestContinueTurnAfterTheAnswerOnlyEnds(t *testing.T) {
	l, st := harnessIn(t, tempDir(t), []scriptedTurn{{text: "the answer"}}, policy.ModeDefault, true)
	if _, err := l.Run(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.Events("sess1")
	var cut int64
	for _, e := range evs {
		if e.Type == EvAgentMessage {
			cut = e.Seq
		}
	}
	st = crashedAt(t, evs, cut)
	evs, _ = st.Events("sess1")
	open, ok := InFlight(evs)
	if !ok || len(open.Calls) != 0 {
		t.Fatalf("InFlight = %+v %v", open, ok)
	}
	ad := &scriptedAdapter{turns: []scriptedTurn{{text: "a second answer"}}}
	rec := NewRecorder(st, "sess1", "")
	rec.Advance(evs[len(evs)-1].Seq)
	l2 := NewLoop(ad, tools.NewRegistry(tools.Read{}), policy.New(policy.ModeDefault), AutoApprove{}, l.Session, rec, DefaultConfig())
	msgs, _ := Fork(evs, 0)
	l2.SetHistory(msgs, 1)
	if reason, err := l2.ContinueTurn(context.Background(), evs, open); err != nil || reason != TermCompleted {
		t.Fatalf("ContinueTurn = %s, %v", reason, err)
	}
	if ad.seen != 0 {
		t.Fatal("the model was asked again after it had answered")
	}
	evs, _ = st.Events("sess1")
	if end, ok := LastEnd(evs); !ok || end.Reason != TermCompleted {
		t.Fatalf("no completed end: %s", types(evs))
	}
}
