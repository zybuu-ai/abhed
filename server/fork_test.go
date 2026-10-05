package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

func seqOfMessage(t *testing.T, events []agent.Event, text string) int64 {
	t.Helper()
	for _, ev := range events {
		if ev.Type == agent.EvUserMessage && strings.Contains(string(ev.Payload), text) {
			return ev.Seq
		}
	}
	t.Fatalf("no user.message %q in the record", text)
	return 0
}

// Forking before the last message and sending an edited one: the record marks
// the fork, the abandoned steps stay in it, and the model is not shown the
// message that was replaced.
func TestForkBeforeAMessageAndSendAnother(t *testing.T) {
	s, st, a, _ := switchServer(t, func(c *config.Config) { c.Auth.Mode = "proxy" })
	id := sessionOf(t, callAs(t, s, "ana", "", "POST", "/v1/sessions", `{"prompt":"first question"}`))
	turnsEnded(t, st, id, 1)
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"second, with a typo"}`); w.Code != http.StatusAccepted {
		t.Fatalf("message: %d %s", w.Code, w.Body.String())
	}
	events := turnsEnded(t, st, id, 2)
	second := seqOfMessage(t, events, "second, with a typo")

	w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/fork", fmt.Sprintf(`{"before_seq":%d}`, second))
	if w.Code != http.StatusOK {
		t.Fatalf("fork: %d %s", w.Code, w.Body.String())
	}
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"second, corrected"}`); w.Code != http.StatusAccepted {
		t.Fatalf("message after the fork: %d %s", w.Code, w.Body.String())
	}
	events = turnsEnded(t, st, id, 3)

	var forks []agent.Forked
	for _, ev := range events {
		if ev.Type == agent.EvForked {
			var f agent.Forked
			_ = json.Unmarshal(ev.Payload, &f)
			if ev.Actor != agent.ActorUser {
				t.Fatalf("the fork is recorded as %s's", ev.Actor)
			}
			forks = append(forks, f)
		}
	}
	if len(forks) != 1 || forks[0].ThroughSeq >= second {
		t.Fatalf("the record's forks: %+v, want one through a step before %d", forks, second)
	}
	seqOfMessage(t, events, "second, with a typo") // the abandoned message stays in the record

	a.mu.Lock()
	last := a.users[len(a.users)-1]
	a.mu.Unlock()
	joined := strings.Join(last, " | ")
	if !strings.Contains(joined, "first question") || !strings.Contains(joined, "second, corrected") || strings.Contains(joined, "typo") {
		t.Fatalf("after the fork the model was sent: %s", joined)
	}
}

// Only the owner forks, only before a message of theirs, and not mid-turn.
func TestForkRefusals(t *testing.T) {
	s, st, a, _ := switchServer(t, func(c *config.Config) { c.Auth.Mode = "proxy" })
	id := sessionOf(t, callAs(t, s, "ana", "", "POST", "/v1/sessions", `{"prompt":"one"}`))
	events := turnsEnded(t, st, id, 1)
	msg := seqOfMessage(t, events, "one")
	notMine := events[len(events)-1].Seq // the session's end

	for _, c := range []struct {
		user, body string
		want       int
	}{
		{"bo", fmt.Sprintf(`{"before_seq":%d}`, msg), http.StatusNotFound},
		{"ana", fmt.Sprintf(`{"before_seq":%d}`, notMine), http.StatusBadRequest},
		{"ana", `{"before_seq":0}`, http.StatusBadRequest},
		{"ana", `{"before_seq":99999}`, http.StatusBadRequest},
	} {
		if w := callAs(t, s, c.user, "", "POST", "/v1/sessions/"+id+"/fork", c.body); w.Code != c.want {
			t.Errorf("%s forking %s: %d %s, want %d", c.user, c.body, w.Code, w.Body.String(), c.want)
		}
	}

	hold := make(chan struct{})
	a.mu.Lock()
	a.hold = hold
	a.mu.Unlock()
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"two"}`); w.Code != http.StatusAccepted {
		t.Fatalf("message: %d %s", w.Code, w.Body.String())
	}
	waitCalls(t, a, 2)
	w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+id+"/fork", fmt.Sprintf(`{"before_seq":%d}`, msg))
	close(hold)
	if w.Code != http.StatusConflict {
		t.Fatalf("a fork mid-turn: %d %s", w.Code, w.Body.String())
	}
	for _, ev := range turnsEnded(t, st, id, 2) {
		if ev.Type == agent.EvForked {
			t.Fatal("a refused fork was recorded")
		}
	}
}
