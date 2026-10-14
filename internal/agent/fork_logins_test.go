package agent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// fakeLogins is a scoped login store that remembers when each was made.
type fakeLogins struct{ at map[string]time.Time }

func (f *fakeLogins) ForgetAfter(t time.Time) {
	for k, v := range f.at {
		if v.After(t) {
			delete(f.at, k)
		}
	}
}

type loginsKey struct{}

// A fork keeps the logins made before its step and drops the ones after it.
func TestForkKeepsLoginsMadeBeforeItsStep(t *testing.T) {
	t0 := time.Now()
	at := func(seq int64, ty EventType, p any, d time.Duration) Event {
		e := ev(seq, ty, p)
		e.CreatedAt = t0.Add(d)
		return e
	}
	events := []Event{
		at(1, EvUserMessage, Message{Text: "log in"}, 0),
		at(2, EvAgentMessage, Message{Text: "logged in"}, 2*time.Second),
		at(3, EvUserMessage, Message{Text: "log in again"}, 4*time.Second),
		at(4, EvAgentMessage, Message{Text: "done"}, 6*time.Second),
	}
	sess, _ := tools.NewSession(tempDir(t))
	logins := &fakeLogins{at: map[string]time.Time{"before": t0.Add(time.Second), "after": t0.Add(5 * time.Second)}}
	sess.Scoped(loginsKey{}, func() any { return logins })
	l := NewLoop(nil, nil, nil, nil, sess, NewRecorder(NewMemStore(), "s", ""), DefaultConfig())
	l.Recorder.Advance(4)
	if _, _, err := l.ForkTo(events, 2); err != nil {
		t.Fatal(err)
	}
	if _, ok := logins.at["before"]; !ok {
		t.Fatal("a login made before the fork point was dropped")
	}
	if _, ok := logins.at["after"]; ok {
		t.Fatal("a login made after the fork point outlived the turns that made it")
	}
}

// A branch's copied session.started names its step in the branch's numbering.
func TestBranchCopyRenumbersThroughSeq(t *testing.T) {
	orig := []Event{
		ev(1, EvUserMessage, Message{Text: "one"}),
		ev(2, EvAgentDelta, Delta{Text: "d"}),
		ev(3, EvAgentMessage, Message{Text: "ok"}),
		ev(4, EvSessionStarted, map[string]any{"resumed": true, "through_seq": 3}),
		ev(5, EvUserMessage, Message{Text: "two"}),
	}
	out := BranchCopy(orig, 0, "b", 10)
	for _, e := range out {
		if e.Type != EvSessionStarted {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		// seq 3 is the second event copied (the delta stays behind), so 11.
		if p["through_seq"] != float64(11) || p["resumed"] != true {
			t.Fatalf("session.started copied as %v, want through_seq 11", p)
		}
		return
	}
	t.Fatal("session.started was not copied")
}
