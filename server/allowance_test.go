package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/server/servertest"
)

// allowanceServer runs sessions whose model calls bash once, then answers,
// asking allow at each turn boundary.
func allowanceServer(t *testing.T, allow func(server.AllowanceRequest) (server.Allowance, error)) (*servertest.Scripted, *servertest.Events, func(who string) string) {
	t.Helper()
	m := &servertest.Scripted{Reply: func(_, _ string, fromTool bool) servertest.Turn {
		if fromTool {
			return servertest.Turn{Text: "done"}
		}
		return servertest.Turn{Tool: "glob", Args: map[string]any{"pattern": "*"}}
	}}
	events := &servertest.Events{}
	s := servertest.Proxy(t, events.Tap, func(o *server.Options) {
		o.Adapter = m
		o.Config.Permissions.Mode = "bypass"
		o.SessionAllowance = func(_ context.Context, req server.AllowanceRequest) (server.Allowance, error) {
			return allow(req)
		}
	})
	start := func(who string) string {
		req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"look"}`))
		req.Header.Set("X-Abhed-User", who)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		var out struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.SessionID == "" {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
		deadline := time.Now().Add(15 * time.Second)
		for events.Count(out.SessionID, "session.ended") == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the session did not end")
			}
			time.Sleep(20 * time.Millisecond)
		}
		return out.SessionID
	}
	return m, events, start
}

func stopOf(t *testing.T, events *servertest.Events, id string) map[string]any {
	t.Helper()
	for _, e := range events.Of(id) {
		if e.Type == "budget.stopped" {
			var p map[string]any
			_ = json.Unmarshal(e.Payload, &p)
			return p
		}
	}
	return nil
}

// The edition's allowance is asked for the session's owner before the first
// turn and at every boundary after: spent at the start, no call is made;
// spent after one turn, the run ends there; each says why.
func TestSessionAllowanceIsAskedAtStartAndEveryTurn(t *testing.T) {
	var mu sync.Mutex
	var asked []server.AllowanceRequest
	spentAfter := 0
	m, events, start := allowanceServer(t, func(req server.AllowanceRequest) (server.Allowance, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, req)
		if len(asked) > spentAfter {
			return server.Allowance{Stop: true, Scope: "user bob, daily", Reason: "stopped: the daily budget of user bob is spent"}, nil
		}
		return server.Allowance{}, nil
	})
	id := start("bob")
	if m.Calls() != 0 {
		t.Fatalf("%d calls past a spent allowance", m.Calls())
	}
	p := stopOf(t, events, id)
	if p == nil || p["reason"] != "allowance" || p["scope"] != "user bob, daily" || p["message"] != "stopped: the daily budget of user bob is spent" {
		t.Fatalf("budget.stopped %v", p)
	}
	mu.Lock()
	if len(asked) != 1 || asked[0].User != "bob" || asked[0].Session != id {
		t.Fatalf("asked %+v", asked)
	}
	asked, spentAfter = nil, 1
	mu.Unlock()

	id = start("bob")
	if m.Calls() != 1 {
		t.Fatalf("%d calls, want 1", m.Calls())
	}
	if stopOf(t, events, id) == nil {
		t.Fatal("no budget.stopped after the turn the allowance ran out in")
	}
}

// An allowance that cannot be read stops the run: no limit is not assumed.
func TestSessionAllowanceFailsClosed(t *testing.T) {
	m, events, start := allowanceServer(t, func(server.AllowanceRequest) (server.Allowance, error) {
		return server.Allowance{}, errors.New("store down")
	})
	id := start("bob")
	if m.Calls() != 0 {
		t.Fatalf("%d calls with the allowance unreadable", m.Calls())
	}
	if p := stopOf(t, events, id); p == nil || p["scope"] != "unavailable" {
		t.Fatalf("budget.stopped %v", p)
	}
}

// Under a dollar allowance a model with no price is refused.
func TestSessionAllowanceInDollarsRefusesUnpriced(t *testing.T) {
	m, events, start := allowanceServer(t, func(server.AllowanceRequest) (server.Allowance, error) {
		return server.Allowance{USD: true, Scope: "workspace pay, monthly"}, nil
	})
	id := start("bob")
	if m.Calls() != 0 {
		t.Fatalf("%d calls to an unpriced model", m.Calls())
	}
	if p := stopOf(t, events, id); p == nil || p["reason"] != "unpriced" {
		t.Fatalf("budget.stopped %v", p)
	}
}
