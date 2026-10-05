//go:build unix

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// revokeOnWake turns the owner inactive the moment a wake run is recorded,
// as an administrator's revoke landing between the wake's start and its
// first model call would.
func revokeOnWake(active *atomic.Bool, mode string) func(*config.Config, *Options) {
	return func(c *config.Config, o *Options) {
		c.Subagents.Wake = mode
		o.OwnerActive = func(context.Context, string, string) bool { return active.Load() }
		o.EventTap = func(ev agent.Event) {
			if ev.Type == agent.EvSessionWoken {
				active.Store(false)
			}
		}
	}
}

// A revoke that lands after the wake started stops it before the model is
// called: the woken run ends owner_inactive and never answers the result.
func TestWakeStopsBeforeModelWhenOwnerRevoked(t *testing.T) {
	var active atomic.Bool
	active.Store(true)
	b := newBGServerWith(t, nil, revokeOnWake(&active, "auto"), "one")
	id := b.start("bg:one", false)
	<-b.ended
	b.ad.release("one")
	waitUntil(t, "the wake run", func() bool { return countType(b.events(id), agent.EvSessionWoken) == 1 })
	waitUntil(t, "the wake's end", func() bool { return b.state(id) == "done" })
	evs := b.events(id)
	woke := false
	for _, e := range evs {
		woke = woke || e.Type == agent.EvSessionWoken
		if woke && (e.Type == agent.EvModelCall || e.Type == agent.EvAgentMessage) {
			t.Fatalf("the woken run reached the model after the revoke: %s %s", e.Type, e.Payload)
		}
	}
	ends := payloadsOf(evs, agent.EvSessionEnded)
	if last := ends[len(ends)-1]; last["reason"] != string(agent.TermOwnerInactive) {
		t.Fatalf("the woken run ended %v", last)
	}
}

// A revoke that lands while a woken run's call is being decided refuses the
// call before it is approved or asked about, and ends the run.
func TestWakeCallRefusedWhenOwnerRevoked(t *testing.T) {
	var active atomic.Bool
	active.Store(true)
	b := newBGServerWith(t, nil, func(c *config.Config, o *Options) {
		c.Subagents.Wake = "auto"
		o.Registry = tools.NewRegistry(tools.Read{}, tools.Bash{})
		o.OwnerActive = func(context.Context, string, string) bool { return active.Load() }
		o.EventTap = func(ev agent.Event) {
			// The woken run's own call is the one named after a notice.
			if ev.Type == agent.EvActionRequested && strings.Contains(string(ev.Payload), `"call_id":"wbgn_`) {
				active.Store(false)
			}
		}
	}, "one")
	b.ad.askOnWake = true
	id := b.start("bg:one", false)
	<-b.ended
	b.ad.release("one")
	waitUntil(t, "the wake run", func() bool { return countType(b.events(id), agent.EvSessionWoken) == 1 })
	waitUntil(t, "the wake's end", func() bool { return b.state(id) == "done" })
	evs := b.events(id)
	for _, a := range payloadsOf(evs, agent.EvActionApproved) {
		if strings.HasPrefix(a["call_id"].(string), "wbgn_") {
			t.Fatalf("the woken run's call was approved after the revoke: %v", a)
		}
	}
	refused := false
	for _, d := range payloadsOf(evs, agent.EvActionDenied) {
		refused = refused || d["step"] == "owner" && strings.HasPrefix(d["call_id"].(string), "wbgn_")
	}
	if !refused {
		t.Fatalf("no refusal for the inactive owner: %v", payloadsOf(evs, agent.EvActionDenied))
	}
	ends := payloadsOf(evs, agent.EvSessionEnded)
	if last := ends[len(ends)-1]; last["reason"] != string(agent.TermOwnerInactive) {
		t.Fatalf("the woken run ended %v", last)
	}
}

// shellAdapter starts a background shell for a message "sh:<name>", and
// answers anything else with text.
type shellAdapter struct {
	mu    sync.Mutex
	calls map[string]int // model calls per first message
}

func (*shellAdapter) Name() string { return "shells" }
func (*shellAdapter) Profile() model.Profile {
	return model.Profile{Name: "shells", ContextWindow: 32000}
}
func (*shellAdapter) CountTokens(model.Request) (int, error) { return 10, nil }
func (a *shellAdapter) Complete(_ context.Context, req model.Request) (<-chan model.Chunk, error) {
	a.mu.Lock()
	a.calls[req.Messages[0].Content]++
	a.mu.Unlock()
	ch := make(chan model.Chunk, 3)
	last := req.Messages[len(req.Messages)-1]
	if last.Role == model.RoleUser && strings.HasPrefix(last.Content, "sh:") {
		args, _ := json.Marshal(map[string]any{"command": "sleep 30", "description": strings.TrimPrefix(last.Content, "sh:"),
			"run_in_background": true})
		ch <- model.Chunk{Type: model.ChunkToolCall, ToolCall: &model.ToolCall{ID: "c1", Name: "bash", Args: args}}
	} else {
		ch <- model.Chunk{Type: model.ChunkText, Text: "ok"}
	}
	ch <- model.Chunk{Type: model.ChunkDone, Usage: &model.Usage{InputTokens: 10}}
	close(ch)
	return ch, nil
}

func (a *shellAdapter) callsFor(first string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[first]
}

// StopOwnerBackground stops one owner's background shells and no one
// else's, records why, and no wake follows for that owner.
func TestStopOwnerBackgroundStopsOnlyThatOwner(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	cfg.Subagents.Wake = "auto"
	cfg.Permissions.Allow = []string{"bash(sleep *)"}
	ad := &shellAdapter{calls: map[string]int{}}
	st := agent.NewMemStore()
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: ad, Registry: tools.NewRegistry(tools.Read{}, tools.Bash{}), Store: st})
	b := &bgServer{t: t, s: s, h: s.Handler(), store: st}
	start := func(user, prompt string) string {
		id, err := s.StartSession(context.Background(), StartSpec{Prompt: prompt, Tenant: "acme", User: user})
		if err != nil {
			t.Fatal(err)
		}
		waitUntil(t, user+"'s shell", func() bool { return b.state(id) == "background" && b.live(id).Loop.Background.Live() == 1 })
		return id
	}
	alice, bob := start("alice", "sh:alice"), start("bob", "sh:bob")
	t.Cleanup(func() { b.live(bob).Loop.Background.CancelAll(agent.TermSessionClosed) })
	before := ad.callsFor("sh:alice")

	if n := s.StopOwnerBackground(context.Background(), "other", "alice"); n != 0 {
		t.Fatalf("another tenant's owner of the same name stopped %d", n)
	}
	if n := s.StopOwnerBackground(context.Background(), "acme", "alice"); n != 1 {
		t.Fatalf("stopped %d, want alice's one shell", n)
	}
	waitUntil(t, "alice's shell ended", func() bool { return countType(b.events(alice), agent.EvShellEnded) == 1 })
	if e := payloadsOf(b.events(alice), agent.EvShellEnded)[0]; e["reason"] != string(agent.TermOwnerRevoked) {
		t.Fatalf("alice's shell ended %v", e)
	}
	if got := b.live(bob).Loop.Background.Live(); got != 1 {
		t.Fatalf("bob's shell stopped too: %d live", got)
	}
	waitUntil(t, "alice's notice", func() bool { return countType(b.events(alice), agent.EvSubagentNotice) == 1 })
	time.Sleep(200 * time.Millisecond)
	if countType(b.events(alice), agent.EvSessionWoken) != 0 || ad.callsFor("sh:alice") != before {
		t.Fatal("a wake ran for an owner whose access was revoked")
	}
}

// A revoke stops a suggestion still being made: none is offered, and nothing
// is recorded once StopOwnerBackground returns, even when the model answers.
func TestStopOwnerBackgroundStopsTheSuggestion(t *testing.T) {
	g := &gatedSuggest{gate: make(chan struct{})}
	b := newBGServerWith(t, nil, func(_ *config.Config, o *Options) { o.Adapter = g })
	id := b.start("hi", false)
	waitUntil(t, "state done", func() bool { return b.state(id) == "done" })
	b.s.StopOwnerBackground(context.Background(), "acme", "alice")
	n := len(b.events(id))
	close(g.gate)
	time.Sleep(300 * time.Millisecond)
	evs := b.events(id)
	if countType(evs, agent.EvSuggestionOffered) != 0 || len(evs) != n {
		t.Fatalf("recorded after the revoke: %d events, then %d", n, len(evs))
	}
}

// A revoke that lands while the run is ending, before or after its
// suggestion is planned, still makes no suggestion call for that owner.
func TestRevokeWhileTheRunEndsMakesNoSuggestion(t *testing.T) {
	for _, at := range []agent.EventType{agent.EvAgentMessage, agent.EvSessionEnded} {
		t.Run(string(at), func(t *testing.T) {
			g := &gatedSuggest{gate: make(chan struct{})}
			close(g.gate)
			var b *bgServer
			b = newBGServerWith(t, nil, func(_ *config.Config, o *Options) {
				o.Adapter = g
				o.EventTap = func(ev agent.Event) {
					if ev.Type == at {
						b.live(ev.SessionID).ownerGone.Store(true)
					}
				}
			})
			id := b.start("hi", false)
			waitUntil(t, "state done", func() bool { return b.state(id) == "done" })
			time.Sleep(300 * time.Millisecond)
			for _, mc := range payloadsOf(b.events(id), agent.EvModelCall) {
				if mc["purpose"] == agent.PurposeSuggestion {
					t.Fatalf("a suggestion call was made for the revoked owner: %v", mc)
				}
			}
		})
	}
}

// After a revoke and a restore, the owner's own next turn brings back the
// wakes and suggestions the revoke held; another person's request does not.
func TestOwnersOwnTurnClearsTheRevokeHold(t *testing.T) {
	b := newBGServer(t, nil)
	id := b.start("hi", false)
	waitUntil(t, "state done", func() bool { return b.state(id) == "done" })
	b.s.StopOwnerBackground(context.Background(), "acme", "alice")
	if ok, why := b.s.canWake(b.live(id)); ok || why != "owner_inactive" {
		t.Fatalf("after the revoke canWake = %v %q", ok, why)
	}
	if rec := b.do("bob", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"again"}`); rec.Code < 400 {
		t.Fatalf("bob's message was taken: %d", rec.Code)
	}
	if !b.live(id).ownerGone.Load() {
		t.Fatal("another person's request cleared the hold")
	}
	if rec := b.do("alice", "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"again"}`); rec.Code != 202 {
		t.Fatalf("alice's message: %d %s", rec.Code, rec.Body)
	}
	if b.live(id).ownerGone.Load() {
		t.Fatal("the owner's own turn left wakes and suggestions held")
	}
	if ok, why := b.s.canWake(b.live(id)); !ok {
		t.Fatalf("after the owner's turn canWake = %v %q", ok, why)
	}
	waitUntil(t, "state done", func() bool { return b.state(id) == "done" })
}

// A revoke ends the owner's workbench terminals, and each one's record says
// why, as the shells and tasks it stops do.
func TestStopOwnerBackgroundNamesTheReasonOnTerminals(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		wb := shellBench(t, func(c *config.Config) { c.Permissions.Allow = []string{"bash(sleep *)"} })
		// An anonymous owner is never revoked, so the session is opened as alice.
		h := wb.h
		wb.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("X-Abhed-User", "alice")
			h.ServeHTTP(w, r)
		})
		wb.session = wb.openIdle("acme")
		var id string
		if interactive {
			id = wb.startShell().ID
		} else {
			id = wb.startPTY("sleep 30").ID
		}
		wb.s.mu.RLock()
		live := wb.s.running[wb.session]
		wb.s.mu.RUnlock()
		if n := wb.s.StopOwnerBackground(context.Background(), live.Tenant, live.User); n != 1 {
			t.Fatalf("interactive=%v: stopped %d, want the one terminal", interactive, n)
		}
		var content string
		waitUntil(t, "the terminal's end", func() bool {
			for _, e := range wb.events() {
				var o agent.Observation
				if e.Type == agent.EvObservation && json.Unmarshal(e.Payload, &o) == nil && o.CallID == id {
					content = o.Content
					return true
				}
			}
			return false
		})
		if !strings.Contains(strings.SplitN(content, "\n", 2)[0], string(agent.TermOwnerRevoked)) {
			t.Fatalf("interactive=%v: a revoked terminal's record does not say why: %q", interactive, content)
		}
	}
}

// A terminal that already ended, kept a minute for a late reader, is not
// counted as stopped by a revoke; one still running is.
func TestStopOwnerBackgroundCountsOnlyRunningTerminals(t *testing.T) {
	wb := shellBench(t, func(c *config.Config) { c.Permissions.Allow = []string{"bash(sleep *)"} })
	h := wb.h
	wb.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Abhed-User", "alice")
		h.ServeHTTP(w, r)
	})
	wb.session = wb.openIdle("acme")
	ended := wb.startPTY("sleep 0").ID
	waitUntil(t, "the short command's end", func() bool {
		for _, e := range wb.events() {
			var o agent.Observation
			if e.Type == agent.EvObservation && json.Unmarshal(e.Payload, &o) == nil && o.CallID == ended {
				return true
			}
		}
		return false
	})
	wb.startPTY("sleep 30")
	wb.s.mu.RLock()
	live := wb.s.running[wb.session]
	wb.s.mu.RUnlock()
	live.mu.Lock()
	lingering := live.ptys[ended] != nil
	live.mu.Unlock()
	if !lingering {
		t.Fatal("the ended command is no longer kept for a late reader")
	}
	if n := wb.s.StopOwnerBackground(context.Background(), live.Tenant, live.User); n != 1 {
		t.Fatalf("stopped %d, want only the running terminal", n)
	}
}
