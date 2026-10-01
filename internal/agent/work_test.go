package agent

import (
	"context"
	"encoding/json"
	"testing"
)

// A background child is listed in the conversation's work while it runs,
// under the conversation, and ends with its status; a message to it is
// queued on its own loop while it runs and refused once it has ended.
func TestWorkFollowsABackgroundChild(t *testing.T) {
	r := newBGRig(t, WakeNotify, "one")
	r.l.Work = NewWork()
	_, _ = r.l.Run(context.Background(), "go")
	var id string
	waitFor(t, "the child listed", func() bool {
		items := r.l.Work.Items()
		if len(items) == 1 {
			id = items[0].ID
		}
		return id != ""
	})
	it, _ := r.l.Work.Item(id)
	if !it.Running() || !it.Background || it.Parent != "parent" || it.Kind != WorkAgent {
		t.Fatalf("listed as %+v", it)
	}
	if err := r.l.Work.Steer(id, "hurry"); err != nil {
		t.Fatalf("a running child refused a message: %v", err)
	}
	r.m.release("one")
	waitFor(t, "the child ended", func() bool {
		it, _ := r.l.Work.Item(id)
		return !it.Running()
	})
	if err := r.l.Work.Steer(id, "late"); err == nil {
		t.Fatal("an ended child took a message")
	}
}

// What a child records moves its activity and tokens; an ended item no
// longer changes, and a message it never took is kept to be said.
func TestWorkObserve(t *testing.T) {
	w := NewWork()
	l := &Loop{}
	w.start(WorkItem{ID: "c", Title: "t"}, l)
	ev := func(typ EventType, v any) Event {
		raw, _ := json.Marshal(v)
		return Event{Type: typ, Payload: raw}
	}
	w.observe("c", ev(EvActionRequested, ActionRequested{Tool: "bash", Args: json.RawMessage(`{"command":"go test"}`)}))
	w.observe("c", ev(EvModelCall, ModelCall{TokensIn: 1200, TokensOut: 30}))
	it, _ := w.Item("c")
	if it.Activity != "bash go test" || it.TokensIn != 1200 || it.TokensOut != 30 {
		t.Fatalf("observed %+v", it)
	}
	w.observe("c", ev(EvObservation, Observation{}))
	if it, _ = w.Item("c"); it.Activity != "thinking" {
		t.Fatalf("activity %q", it.Activity)
	}
	w.end("c", "completed", "completed", []string{"left"})
	w.observe("c", ev(EvModelCall, ModelCall{TokensIn: 5}))
	it, _ = w.Item("c")
	if it.Running() || it.TokensIn != 1200 || it.Activity != "" || len(it.Undelivered) != 1 {
		t.Fatalf("after its end %+v", it)
	}
	// A resumed run keeps its title and tokens.
	w.start(WorkItem{ID: "c"}, l)
	if it, _ = w.Item("c"); !it.Running() || it.Title != "t" || it.TokensIn != 1200 {
		t.Fatalf("resumed as %+v", it)
	}
}
