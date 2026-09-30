package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// eventsOf are the payloads of the events of type t, decoded into T.
func eventsOf[T any](t *testing.T, evs []agent.Event, typ agent.EventType) []T {
	t.Helper()
	var out []T
	for _, ev := range evs {
		if ev.Type != typ {
			continue
		}
		var p T
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// Through the CLI: a mode chosen before the first message lands in that
// conversation's record ahead of it, /mode auto takes a typed yes, and a
// refused answer changes nothing.
func TestCLIModeChangesReachTheRecord(t *testing.T) {
	c := startCLI(t)
	c.command("/mode plan", "mode: plan")
	c.command("/mode auto", "answer 1-2")
	c.command("no", "mode stays plan")
	c.command("/mode auto", "answer 1-2")
	c.command("yes", "mode: auto")
	c.task("hello")
	evs := c.export()
	var got []string
	for _, mc := range eventsOf[agent.ModeChanged](t, evs, agent.EvModeChanged) {
		got = append(got, mc.From+">"+mc.To+"/"+mc.Via)
	}
	if want := []string{"default>plan/slash", "plan>auto/slash"}; !slices.Equal(got, want) {
		t.Fatalf("mode changes %v, want %v", got, want)
	}
	types := (func() []agent.EventType {
		var ts []agent.EventType
		for _, e := range evs {
			ts = append(ts, e.Type)
		}
		return ts
	})()
	if first := slices.Index(types, agent.EvUserMessage); first < slices.Index(types, agent.EvModeChanged) {
		t.Fatalf("the mode change was recorded after the message it preceded: %v", types)
	}
}

// sseCall streams one tool call as the nth reply.
func sseCall(w io.Writer, n int, name, args string) {
	fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+strconv.Itoa(n)+
		`","type":"function","function":{"name":"`+name+`","arguments":`+strconv.Quote(args)+`}}]}}]}`)
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
}

// The interactive CLI's turn limit is per message when the configuration
// is the person's own: a message that ran out is told how to go on, and the
// next one gets its own turns.
func TestCLITurnLimitIsPerMessage(t *testing.T) {
	c := startCLIConfig(t, func(w io.Writer, n int, _ string) { sseCall(w, n, "glob", `{"pattern":"*.none"}`) },
		func(url string) string {
			return `{"limits":{"max_turns":2},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
				url + `","model":"m","context_window":8192}}}}`
		})
	fmt.Fprintln(c.stdin, "first")
	c.waitFor(func(out string) bool { return strings.Contains(out, "stopped after 2 turns for this message") }, "the per-message note")
	fmt.Fprintln(c.stdin, "continue")
	c.waitFor(func(out string) bool { return strings.Count(out, "stopped after 2 turns for this message") == 2 }, "the second message's turns")
	if got := c.requests(); got != 4 {
		t.Fatalf("the model was asked %d times, want 2 per message", got)
	}
}

// A managed limits.max_turns bounds the whole conversation, and says so; a
// limit of the person's own is per message.
func TestTurnLimitFollowsTheManagedConfiguration(t *testing.T) {
	own := config.Default()
	own.Limits.MaxTurns = 30
	managed := own
	managed.Managed, managed.ManagedKeys = true, []string{"limits.max_turns"}
	if turnsPerMessage(own, 30) != 30 || turnsPerMessage(managed, 30) != 0 || turnsPerMessage(own, 0) != 0 {
		t.Fatal("turnsPerMessage does not follow the managed configuration")
	}
	if !strings.Contains(turnLimitSummary(managed, 30), "whole conversation, set by the managed configuration") ||
		!strings.Contains(turnLimitSummary(own, 30), "30 for each message") {
		t.Fatal("the summary does not say which limit applies")
	}
	if !strings.Contains(turnLimitNote(managed, 30), "/clear starts a new one") || !strings.Contains(turnLimitNote(own, 30), "continue") {
		t.Fatal("the end note does not say how to go on")
	}
}

// Through the CLI: an edit of a file not read this session is never put to
// the person, and the model is told to read it first.
func TestCLIUnreadEditIsNotAsked(t *testing.T) {
	var ws atomic.Value
	c := startCLIConfig(t, func(w io.Writer, n int, _ string) {
		if n == 1 {
			sseCall(w, n, "edit", `{"path":`+strconv.Quote(filepath.Join(ws.Load().(string), "main.go"))+`,"old_string":"a","new_string":"b"}`)
			return
		}
		textReply("ok")(w, n)
	}, func(url string) string {
		return `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + url + `","model":"m","context_window":8192}}}}`
	})
	ws.Store(c.ws)
	if err := os.WriteFile(filepath.Join(c.ws, "main.go"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.task("change main.go")
	if strings.Contains(c.out.String(), "[a]ccept") {
		t.Fatalf("the person was asked to approve an edit that could not succeed:\n%s", c.out.String())
	}
	c.mu.Lock()
	second := c.bodies[1]
	c.mu.Unlock()
	if !strings.Contains(second, "has not been read this session") {
		t.Fatalf("the model was not told to read first: %s", second)
	}
}
