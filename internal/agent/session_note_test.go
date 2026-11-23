package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// The session's facts are the system prompt's last section from the first
// message on, never part of a message; a change reaches the next request and
// is recorded once.
func TestSessionFactsGoInTheSystemPrompt(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{{text: "hello"}, {text: "again"}, {text: "third"}}, policy.ModeDefault, true)
	note := "network off"
	l.SessionNote = func() string { return note }
	ad := l.Adapter.(*scriptedAdapter)
	for _, p := range []string{"hi", "and again"} {
		if _, err := l.Run(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	for i, req := range ad.gotRequests {
		if !strings.HasSuffix(req.System, "Session: network off") {
			t.Fatalf("request %d system: %q", i, req.System)
		}
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "network off") {
				t.Fatalf("a message carries the facts: %q", m.Content)
			}
		}
	}
	note = "network on"
	if _, err := l.Run(context.Background(), "now?"); err != nil {
		t.Fatal(err)
	}
	if last := ad.gotRequests[len(ad.gotRequests)-1]; !strings.HasSuffix(last.System, "network on") {
		t.Fatalf("a changed note was not given: %q", last.System)
	}
	evs, _ := store.Events("sess1")
	n := 0
	for _, e := range evs {
		if e.Type == EvSessionFacts {
			n++
		}
		if e.Type == EvUserMessage && strings.Contains(string(e.Payload), "network") {
			t.Fatalf("a user message carries the facts: %s", e.Payload)
		}
	}
	if n != 2 {
		t.Fatalf("recorded %d times, want once per change", n)
	}
}

// A subagent is never given the note: it answers to the session that has it.
func TestSessionFactsNotForSubagents(t *testing.T) {
	l, _, _ := harness(t, []scriptedTurn{{text: "ok"}}, policy.ModeDefault, true)
	l.SessionNote = func() string { return "facts" }
	l.depth = 1
	l.refreshFacts()
	if l.system() != l.Config.SystemPrompt {
		t.Fatal("a subagent got the facts")
	}
}
