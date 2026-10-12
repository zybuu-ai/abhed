package hawkeye

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func event(seq int64, ty agent.EventType, p any) agent.Event {
	b, _ := json.Marshal(p)
	return agent.Event{Seq: seq, Type: ty, Payload: b, CreatedAt: time.Unix(seq, 0)}
}

func hasFinding(r Report, code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// A provider that gives no cached figure is not a cold cache: the report says so.
func TestCacheNotReportedIsNotZero(t *testing.T) {
	evs := []agent.Event{
		event(1, agent.EvUserMessage, agent.Message{Text: "hi"}),
		event(2, agent.EvModelCall, agent.ModelCall{Turn: 1, TokensIn: 100, TokensOut: 5}),
		event(3, agent.EvSessionEnded, agent.SessionEnded{Reason: agent.TermCompleted}),
	}
	r := Analyze("s", evs)
	if r.Totals.CacheReported || !strings.Contains(Text(r), "cache not reported") || strings.Contains(Text(r), "0% cached") {
		t.Fatalf("text = %s", Text(r))
	}
	html, err := HTML(r)
	if err != nil || !strings.Contains(html, "cache not reported") {
		t.Fatal("the page does not say the cache was not reported")
	}
	evs[1] = event(2, agent.EvModelCall, agent.ModelCall{Turn: 1, TokensIn: 100, TokensOut: 5, CacheReported: true})
	if r := Analyze("s", evs); !r.Totals.CacheReported || !strings.Contains(Text(r), "0% cached") {
		t.Fatalf("a reported zero is a cold cache: %s", Text(r))
	}
}

// A session open with no run going is idle, not running.
func TestOpenSessionWithNoRunIsIdle(t *testing.T) {
	evs := []agent.Event{
		event(1, agent.EvSessionStarted, map[string]any{"origin": "workbench"}),
		event(2, agent.EvUserMessage, agent.Message{Text: "hi"}),
		event(3, agent.EvModelCall, agent.ModelCall{Turn: 1, TokensIn: 10}),
	}
	if r := AnalyzeWith("s", evs, Options{Live: true, Idle: true}); r.Outcome != "idle" {
		t.Fatalf("outcome = %s, want idle", r.Outcome)
	}
	if r := AnalyzeWith("s", evs, Options{Live: true}); r.Outcome != "running" {
		t.Fatalf("outcome = %s, want running", r.Outcome)
	}
}

// What the sandbox recorded inside a command shows as findings.
func TestSandboxEventsRaiseFindings(t *testing.T) {
	evs := []agent.Event{
		event(1, agent.EvUserMessage, agent.Message{Text: "hi"}),
		event(2, agent.EvShellEnded, agent.ShellEnded{ShellID: "sh1", State: agent.ShellKilled, Reason: "owner_revoked"}),
		event(3, agent.EvFenceLimit, map[string]any{"call_id": "c1", "pids_max": 1}),
		event(4, agent.EvFenceStatePlanted, map[string]any{"call_id": "c2", "reason": "a .abhed appeared"}),
		event(5, agent.EvSessionEnded, agent.SessionEnded{Reason: agent.TermCompleted}),
	}
	r := Analyze("s", evs)
	for _, code := range []string{"shell-owner-gone", "fence-limit", "fence-state-planted"} {
		if !hasFinding(r, code) {
			t.Errorf("no %s finding: %+v", code, r.Findings)
		}
	}
}
