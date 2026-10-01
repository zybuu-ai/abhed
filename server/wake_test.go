package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A wake that finds nothing to do is no failure: the session keeps the end
// it had, and is not marked as ended in error.
func TestServerWakeWithNothingToDoIsBenign(t *testing.T) {
	b := newBGServer(t, nil)
	id := b.start("hello", false)
	<-b.ended
	live := b.live(id)
	live.mu.Lock()
	prior := live.Reason
	live.mu.Unlock()
	if !b.s.wake(live, nil) {
		t.Fatal("the wake was not started")
	}
	waitUntil(t, "the wake's end", func() bool { live.mu.Lock(); defer live.mu.Unlock(); return live.ran == nil })
	live.mu.Lock()
	reason, state := live.Reason, live.State
	live.mu.Unlock()
	if reason != prior || reason == agent.TermError || state != "done" {
		t.Fatalf("after a wake with nothing to do: reason %q (was %q), state %q", reason, prior, state)
	}
}

// openStream opens the session's event stream and sends its lines on the
// channel returned, which closes when the stream does.
func (b *bgServer) openStream(id string) <-chan string {
	b.t.Helper()
	srv := httptest.NewServer(b.h)
	b.t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions/"+id+"/events", nil)
	req.Header.Set("X-Abhed-Tenant", "acme")
	req.Header.Set("X-Abhed-User", "alice")
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the reader once the stream ends
	if err != nil {
		b.t.Fatal(err)
	}
	lines := make(chan string, 512)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	return lines
}

// With nothing configured, a result that arrives while the session is idle
// starts a turn on its own: the stream already open carries session.woken
// naming the task, then the model's reply to the result, then the end.
func TestServerWakesByDefault(t *testing.T) {
	b := newBGServer(t, nil, "one")
	id := b.start("bg:one", false)
	<-b.ended
	waitUntil(t, "state background", func() bool { return b.state(id) == "background" })
	lines := b.openStream(id)
	b.ad.release("one")
	var woken, replied, settled bool
	timeout := time.After(15 * time.Second)
	for !settled {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("the stream closed first: woken %v, replied %v", woken, replied)
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev agent.Event
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
				continue
			}
			switch ev.Type {
			case agent.EvSessionWoken:
				var w agent.SessionWoken
				_ = json.Unmarshal(ev.Payload, &w)
				woken = len(w.TaskIDs) == 1 && w.By == "policy"
			case agent.EvAgentMessage:
				replied = replied || woken && strings.Contains(string(ev.Payload), "acting on:") && strings.Contains(string(ev.Payload), "result of one")
			case agent.EvSessionEnded:
				settled = woken
			}
		case <-timeout:
			t.Fatalf("no woken turn on the stream: woken %v, replied %v", woken, replied)
		}
	}
	if !woken || !replied {
		t.Fatalf("woken %v, replied to the result %v", woken, replied)
	}
	waitUntil(t, "state done", func() bool { return b.state(id) == "done" })
	n := payloadsOf(b.events(id), agent.EvSubagentNotice)
	if len(n) != 1 || n[0]["delivery"] != "wake" || n[0]["wake"] != "auto" {
		t.Fatalf("notice: %v", n)
	}
}

// A result that arrives while a turn is running is delivered at that run's
// next boundary, inside it: no wake, and the run answers it before it ends.
func TestServerResultMidRunDeliveredAtBoundary(t *testing.T) {
	b := newBGServer(t, nil, "one")
	b.ad.slow = 600 * time.Millisecond
	id := b.start("bg:one", false)
	waitUntil(t, "the child", func() bool { return b.live(id).Loop.Background.Live() == 1 })
	b.ad.release("one")
	<-b.ended
	waitUntil(t, "state done", func() bool { return b.state(id) == "done" })
	n := payloadsOf(b.events(id), agent.EvSubagentNotice)
	if len(n) != 1 || n[0]["delivery"] != "boundary" {
		t.Fatalf("notice: %v", n)
	}
	if countType(b.events(id), agent.EvSessionWoken) != 0 {
		t.Fatal("a wake ran for a result the live run took")
	}
	var answered bool
	for _, m := range payloadsOf(b.events(id), agent.EvAgentMessage) {
		answered = answered || strings.Contains(fmt.Sprint(m["text"]), "result of one")
	}
	if !answered {
		t.Fatal("the run ended without answering the result")
	}
}

// Stop between a result arriving and its wake holds the wake: the result is
// recorded as skipped, and no turn starts.
func TestServerStopPreventsWake(t *testing.T) {
	b := newBGServer(t, nil, "one")
	id := b.start("bg:one", false)
	<-b.ended
	live := b.live(id)
	waitUntil(t, "state background", func() bool { return b.state(id) == "background" })
	b.ad.release("one")
	waitUntil(t, "the result", func() bool { return live.Loop.Background.Live() == 0 })
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/interrupt", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("interrupt: %d", rec.Code)
	}
	waitUntil(t, "the notice", func() bool { return countType(b.events(id), agent.EvSubagentNotice) == 1 })
	if n := payloadsOf(b.events(id), agent.EvSubagentNotice)[0]; n["wake"] != "skipped:stopped" {
		t.Fatalf("notice: %v", n)
	}
	time.Sleep(200 * time.Millisecond)
	if countType(b.events(id), agent.EvSessionWoken) != 0 {
		t.Fatal("a wake ran after Stop")
	}
}

// The hourly cap holds: once it is used, a later result is delivered
// without a wake, as skipped:wake_limit.
func TestServerWakeCapHonoured(t *testing.T) {
	b := newBGServerWith(t, nil, func(c *config.Config, _ *Options) { c.Subagents.MaxWakesPerHour = 1 }, "one", "two")
	id := b.start("bg:one", false)
	<-b.ended
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"bg:two"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("message: %d", rec.Code)
	}
	waitUntil(t, "two running", func() bool { return b.live(id).Loop.Background.Live() == 2 && b.state(id) == "background" })
	b.ad.release("one")
	waitUntil(t, "the first wake", func() bool { return countType(b.events(id), agent.EvSessionWoken) == 1 })
	waitUntil(t, "the wake's end", func() bool { return b.state(id) == "background" })
	b.ad.release("two")
	waitUntil(t, "the second notice", func() bool { return countType(b.events(id), agent.EvSubagentNotice) == 2 })
	if n := payloadsOf(b.events(id), agent.EvSubagentNotice)[1]; n["wake"] != "skipped:wake_limit" {
		t.Fatalf("second notice: %v", n)
	}
	time.Sleep(200 * time.Millisecond)
	if c := countType(b.events(id), agent.EvSessionWoken); c != 1 {
		t.Fatalf("%d wakes with a cap of one", c)
	}
}
