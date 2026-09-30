package server

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func autoWake(c *config.Config, _ *Options) { c.Subagents.Wake = "auto" }

// With wake auto an idle result starts a wake run on the server: the record
// says it woke, the result arrives at the wake's boundary, and the session
// settles done.
func TestServerAutoWake(t *testing.T) {
	b := newBGServerWith(t, nil, autoWake, "one")
	id := b.start("bg:one", false)
	<-b.ended
	b.ad.release("one")
	waitUntil(t, "the wake run", func() bool { return countType(b.events(id), agent.EvSessionWoken) == 1 })
	waitUntil(t, "the closing state", func() bool { return b.state(id) == "done" })
	n := payloadsOf(b.events(id), agent.EvSubagentNotice)
	if len(n) != 1 || n[0]["delivery"] != "wake" || n[0]["wake"] != "auto" {
		t.Fatalf("notice: %v", n)
	}
}

// No wake starts while the server drains: the result is recorded as skipped.
func TestNoWakeWhileDraining(t *testing.T) {
	b := newBGServerWith(t, nil, autoWake, "one")
	id := b.start("bg:one", false)
	<-b.ended
	b.s.draining.Store(true)
	b.ad.release("one")
	waitUntil(t, "the notice", func() bool { return countType(b.events(id), agent.EvSubagentNotice) == 1 })
	if n := payloadsOf(b.events(id), agent.EvSubagentNotice)[0]; n["wake"] != "skipped:draining" {
		t.Fatalf("notice: %v", n)
	}
	if countType(b.events(id), agent.EvSessionWoken) != 0 {
		t.Fatal("a wake ran while draining")
	}
}

// An owner no longer active gets no wake, and the session's other children
// are cancelled as owner_inactive, in notify mode as in auto: nobody may
// answer their asks.
func TestWakeSkippedOwnerInactive(t *testing.T) {
	for _, mode := range []string{"auto", "notify"} {
		b := newBGServerWith(t, nil, func(c *config.Config, o *Options) {
			c.Subagents.Wake = mode
			o.OwnerActive = func(context.Context, string, string) bool { return false }
		}, "one", "two")
		id := b.start("bg:one", false)
		<-b.ended
		if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"bg:two"}`); rec.Code != http.StatusAccepted {
			t.Fatalf("%s: message: %d", mode, rec.Code)
		}
		waitUntil(t, "two running", func() bool { return b.live(id).Loop.Background.Live() == 2 && b.state(id) == "background" })
		b.ad.release("one")
		waitUntil(t, mode+": two cancelled", func() bool {
			for _, r := range payloadsOf(b.events(id), agent.EvSubagentReturn) {
				if r["description"] == "two" && r["reason"] == string(agent.TermOwnerInactive) {
					return true
				}
			}
			return false
		})
		if countType(b.events(id), agent.EvSessionWoken) != 0 {
			t.Fatalf("%s: a wake ran for an inactive owner", mode)
		}
	}
}

// The owner lookup, which may take seconds, is never made while the
// conversation is locked: anything needing the run lock goes on meanwhile.
func TestOwnerLookupOutsideTheRunLock(t *testing.T) {
	var live atomic.Pointer[liveSession]
	unlocked := make(chan bool, 4)
	b := newBGServerWith(t, nil, func(c *config.Config, o *Options) {
		c.Subagents.Wake = "auto"
		o.OwnerActive = func(context.Context, string, string) bool {
			l := live.Load()
			done := make(chan struct{})
			go func() { l.Loop.SetHistory(l.Loop.Messages(), 0); close(done) }()
			select {
			case <-done:
				unlocked <- true
			case <-time.After(2 * time.Second):
				unlocked <- false
			}
			return true
		}
	}, "one")
	id := b.start("bg:one", false)
	<-b.ended
	live.Store(b.live(id))
	b.ad.release("one")
	select {
	case ok := <-unlocked:
		if !ok {
			t.Fatal("the owner lookup ran with the run lock held")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the owner was never looked up")
	}
	waitUntil(t, "the wake run", func() bool { return countType(b.events(id), agent.EvSessionWoken) == 1 })
}

// A wake run's ask waits for the owner like any other; nothing approves it.
func TestWakeAskNeverAutoGranted(t *testing.T) {
	b := newBGServerWith(t, nil, func(c *config.Config, o *Options) {
		c.Subagents.Wake = "auto"
		o.Registry = tools.NewRegistry(tools.Read{}, tools.Bash{})
	}, "one")
	b.ad.askOnWake = true
	id := b.start("bg:one", false)
	<-b.ended
	b.ad.release("one")
	waitUntil(t, "the wake's ask", func() bool { return b.state(id) == "waiting_approval" })
	time.Sleep(100 * time.Millisecond)
	for _, a := range payloadsOf(b.events(id), agent.EvActionApproved) {
		if a["call_id"] != "tone" { // the task call the policy allowed on its own
			t.Fatalf("a wake run's ask was approved with nobody asked: %v", a)
		}
	}
	if rec := b.do("mallory", "POST", "/v1/sessions/"+id+"/approve", `{"approved":true}`); rec.Code == http.StatusNoContent || rec.Code == http.StatusOK {
		t.Fatalf("another user answered the wake's ask: %d", rec.Code)
	}
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/approve", `{"approved":false}`); rec.Code >= 300 {
		t.Fatalf("the owner's answer: %d %s", rec.Code, rec.Body)
	}
	waitUntil(t, "done", func() bool { return b.state(id) == "done" })
}

// With local accounts, an owner whose account is gone is not active.
func TestLocalOwnerActive(t *testing.T) {
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	if err := local.CreateUser(context.Background(), auth.User{Username: "alice", Email: "alice@example.com"}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Registry: tools.NewRegistry(tools.Read{}),
		Auth: &auth.Middleware{Providers: []auth.Provider{local}}})
	for user, want := range map[string]bool{"alice": true, "alice@example.com": true, "bob": false} {
		if got := s.ownerActive(&liveSession{User: user}); got != want {
			t.Fatalf("%s active = %v", user, got)
		}
	}
}

// A wake run that stopped at its cap with a person's message queued after
// its last look runs again for the message, as a completed run does.
func TestWakeLimitWithQueuedMessageRunsOn(t *testing.T) {
	b := newBGServer(t, nil)
	id := b.start("hello", false)
	<-b.ended
	live := b.live(id)
	live.Loop.QueueMessage(agent.Message{Text: "and this"})
	if !live.settle(context.Background(), agent.TermWakeLimit, nil) {
		t.Fatal("a wake run's end left the person's message queued")
	}
	live.Loop.QueueMessage(agent.Message{Text: "x"})
	if live.settle(context.Background(), agent.TermUserInterrupt, nil) {
		t.Fatal("an interrupted run ran on")
	}
}
