package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// The session note goes with the first message, not again while it is the
// same, and again once it has changed, such as after the network opened.
func TestSessionNoteGoesWithTheFirstMessage(t *testing.T) {
	l, _, _ := harness(t, []scriptedTurn{{text: "hello"}, {text: "again"}, {text: "third"}}, policy.ModeDefault, true)
	note := "[Session facts] network off"
	l.SessionNote = func() string { return note }
	ad := l.Adapter.(*scriptedAdapter)
	for _, p := range []string{"hi", "and again"} {
		if _, err := l.Run(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	first := ad.gotRequests[0].Messages
	if got := first[len(first)-1].Content; !strings.HasPrefix(got, "hi\n\n[Session facts]") {
		t.Fatalf("first message: %q", got)
	}
	second := ad.gotRequests[1].Messages
	if got := second[len(second)-1].Content; got != "and again" {
		t.Fatalf("given again unchanged: %q", got)
	}
	note = "[Session facts] network on"
	if _, err := l.Run(context.Background(), "now?"); err != nil {
		t.Fatal(err)
	}
	third := ad.gotRequests[2].Messages
	if got := third[len(third)-1].Content; !strings.Contains(got, "network on") {
		t.Fatalf("a changed note was not given: %q", got)
	}
}

// A subagent is never given the note: it answers to the session that has it.
func TestSessionNoteNotForSubagents(t *testing.T) {
	l, _, _ := harness(t, []scriptedTurn{{text: "ok"}}, policy.ModeDefault, true)
	l.SessionNote = func() string { return "facts" }
	l.depth = 1
	if n := l.sessionNote(); n != "" {
		t.Fatalf("a subagent got %q", n)
	}
}

// A history set again, as a resume or a compaction does, is given the
// session facts with its next message.
func TestSessionNoteGivenAgainAfterHistoryIsReplaced(t *testing.T) {
	l, _, _ := harness(t, []scriptedTurn{{text: "ok"}}, policy.ModeDefault, true)
	l.SessionNote = func() string { return "facts" }
	if l.sessionNote() != "facts" || l.sessionNote() != "" {
		t.Fatal("given once")
	}
	l.SetHistory(nil, 0)
	if l.sessionNote() != "facts" {
		t.Fatal("not given again after the history was replaced")
	}
}
