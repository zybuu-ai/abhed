package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
)

// withExtensions starts the named scripts from the extension package's
// fixtures and hands them to the server as a running host.
func withExtensions(t *testing.T, scripts ...string) func(*Options) {
	t.Helper()
	h := extension.NewHost(nil)
	cfgs := make([]extension.Config, 0, len(scripts))
	for _, s := range scripts {
		cfgs = append(cfgs, extension.Config{Name: s, Command: "bash",
			Args: []string{filepath.Join("..", "internal", "extension", "testdata", s)}, Timeout: 5 * time.Second})
	}
	if errs := h.Load(context.Background(), cfgs); len(errs) > 0 {
		t.Fatalf("load: %v", errs)
	}
	t.Cleanup(h.Close)
	return func(o *Options) { o.Extensions = h }
}

// hookDenial returns the denial of the call with the given command.
func hookDenial(t *testing.T, evs []agent.Event) map[string]string {
	t.Helper()
	for _, ev := range evs {
		if ev.Type == agent.EvActionDenied {
			var d map[string]string
			_ = json.Unmarshal(ev.Payload, &d)
			return d
		}
	}
	t.Fatalf("nothing was denied: %v", typesOf(evs))
	return nil
}

// A veto extension on the server refuses a console session's own call.
func TestServerExtensionVetoesASessionsCall(t *testing.T) {
	s, ws := scriptedServer(t, [2]string{"bash", `{"command":"echo secret > leaked.txt"}`}, [2]string{}, nil,
		withExtensions(t, "blocker.sh"), func(c *config.Config) { c.Permissions.Mode = "bypass" })
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"go"}`))
	evs := eventsUntil(t, s, id, agent.EvSessionEnded)
	if d := hookDenial(t, evs); d["step"] != "hook" || d["reason"] != "touches a secret" {
		t.Fatalf("denial %v", d)
	}
	if _, err := os.Stat(filepath.Join(ws, "leaked.txt")); err == nil {
		t.Fatal("the vetoed command ran")
	}
}

// The veto reaches the session's subagents, and the parent's record says so.
func TestServerExtensionVetoesASubagentsCall(t *testing.T) {
	s, ws := scriptedServer(t, delegate, [2]string{"bash", `{"command":"echo secret > leaked.txt"}`}, nil,
		withExtensions(t, "blocker.sh"), func(c *config.Config) { c.Permissions.Mode = "bypass" })
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"go"}`))
	evs := eventsUntil(t, s, id, agent.EvSessionEnded)
	got := payloadOf[agent.SubagentAction](t, evs, agent.EvSubagentAction)
	if got.Decision != "denied" || got.Step != "hook" || got.Reason != "touches a secret" {
		t.Fatalf("the parent's record does not show the veto: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "leaked.txt")); err == nil {
		t.Fatal("the subagent's vetoed command ran")
	}
}

// An extension answering "allow" to everything cannot turn an ask into an
// allow: the ask-rule call waits for the person, and runs only on their yes.
func TestServerExtensionCannotAllowAnAsk(t *testing.T) {
	s, ws := scriptedServer(t, [2]string{"bash", `{"command":"touch made.txt"}`}, [2]string{}, nil,
		withExtensions(t, "evil.sh"))
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"go"}`))
	evs := eventsUntil(t, s, id, agent.EvActionRequested)
	req := payloadOf[agent.ActionRequested](t, evs, agent.EvActionRequested)
	if !req.RequiresApproval {
		t.Fatalf("the ask-rule call was not put to the person: %+v", req)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(ws, "made.txt")); err == nil {
		t.Fatal("the ask-rule call ran unanswered")
	}
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/approve", `{"approved":false}`); w.Code != http.StatusNoContent {
		t.Fatalf("answer %d %s", w.Code, w.Body)
	}
	evs = eventsUntil(t, s, id, agent.EvSessionEnded)
	if d := hookDenial(t, evs); d["by"] != agent.ByReviewer {
		t.Fatalf("the call was settled by %v, not the person", d)
	}
}

// Compaction on the server asks the extensions for the summary.
func TestServerCompactionAsksTheExtensions(t *testing.T) {
	s, _ := scriptedServer(t, [2]string{}, [2]string{}, nil, withExtensions(t, "summarizer.sh"))
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"prompt":"first"}`))
	eventsUntil(t, s, id, agent.EvSessionEnded)
	if w := call(t, s, "POST", "/v1/sessions/"+id+"/messages", `{"prompt":"second"}`); w.Code >= 300 {
		t.Fatalf("continue %d %s", w.Code, w.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for ends := 0; ends < 2 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		ends = 0
		evs, _ := s.store.Events(id)
		for _, ev := range evs {
			if ev.Type == agent.EvSessionEnded {
				ends++
			}
		}
	}
	s.mu.RLock()
	live := s.running[id]
	s.mu.RUnlock()
	info, err := live.Loop.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Summary, "ABC-123") {
		t.Fatalf("the summary was not the extension's: %q", info.Summary)
	}
}

// The capabilities say which configured extensions' veto is in force: a
// running one, one that stopped, and one that never started.
func TestCapabilitiesReportExtensionStatus(t *testing.T) {
	s, _ := scriptedServer(t, [2]string{}, [2]string{}, nil, withExtensions(t, "blocker.sh"), func(c *config.Config) {
		c.Extensions = []config.ExtensionConfig{{Name: "blocker.sh", Command: "bash"}, {Name: "gone.sh", Command: "bash"}}
	})
	var c capabilities
	_ = json.Unmarshal(call(t, s, "GET", "/v1/capabilities", "").Body.Bytes(), &c)
	got := map[string]string{}
	for _, e := range c.Extensions {
		got[e.Name] = e.Status
	}
	if got["blocker.sh"] != "running" || got["gone.sh"] != "not started" {
		t.Fatalf("extension status %v", got)
	}
	s.opts.Extensions.Close() // as a crash leaves it: no longer asked
	_ = json.Unmarshal(call(t, s, "GET", "/v1/capabilities", "").Body.Bytes(), &c)
	for _, e := range c.Extensions {
		if e.Name == "blocker.sh" && e.Status != "stopped" {
			t.Fatalf("a closed extension reads as %q", e.Status)
		}
	}
}
