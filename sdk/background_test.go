package abhed_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// bgModel starts one background task ("child") on a prompt starting "go";
// the child waits for release, or runs command first when one is set.
func bgModel(t *testing.T, release <-chan struct{}, command string) string {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		first := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				first = m.Content
				break
			}
		}
		last := req.Messages[len(req.Messages)-1]
		w.Header().Set("Content-Type", "text/event-stream")
		text := func(s string) {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", s)
		}
		call := func(name, args string) {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+strconv.FormatInt(n.Add(1), 10)+
				`","type":"function","function":{"name":"`+name+`","arguments":`+strconv.Quote(args)+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		}
		switch {
		case first == "child" && last.Role == "user" && command != "":
			call("bash", `{"command":`+strconv.Quote(command)+`}`)
		case first == "child":
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			text("child result")
		case last.Role == "tool" && strings.HasPrefix(last.ToolCallID, "bgn_"):
			text("noted")
		case last.Role == "user" && strings.HasPrefix(last.Content, "go"):
			call("task", `{"prompt":"child","description":"child","background":true}`)
		default:
			text("done")
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func bgAgent(t *testing.T, url, mode string, approve func(context.Context, string, json.RawMessage, abhed.Decision) (bool, error), onEvent func(abhed.Event)) *abhed.Agent {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	a, err := abhed.New(context.Background(), abhed.Options{Workspace: t.TempDir(), ConfiguredTools: true, Background: mode,
		Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: url, Model: "m", ContextWindow: 8192},
		Approve:  approve, OnEvent: onEvent})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// By default an embedded agent joins its background tasks: Run returns
// when the work, the child's included, is done.
func TestSDKDefaultJoins(t *testing.T) {
	release := make(chan struct{})
	a := bgAgent(t, bgModel(t, release, ""), "", nil, nil)
	defer a.Close()
	go func() { time.Sleep(200 * time.Millisecond); close(release) }()
	answer, err := a.Run(context.Background(), "go")
	if err != nil || answer != "noted" || len(a.Background()) != 1 || a.Background()[0].Status != "completed" {
		t.Fatalf("Run: %q %v %+v", answer, err, a.Background())
	}
}

// In notify a task outlives Run; its result reaches OnEvent when it ends,
// and Wake runs the agent on it, recorded as woken by the caller.
func TestSDKNotifyThenWakeRecordsCaller(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	a := bgAgent(t, bgModel(t, release, ""), "notify", nil, func(ev abhed.Event) {
		mu.Lock()
		seen = append(seen, string(ev.Type))
		mu.Unlock()
	})
	defer a.Close()
	if answer, err := a.Run(context.Background(), "go"); err != nil || answer != "done" {
		t.Fatalf("Run: %q %v", answer, err)
	}
	if len(a.Background()) != 1 || a.Background()[0].Status != "running" {
		t.Fatalf("tasks: %+v", a.Background())
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.WaitBackground(ctx); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		got := strings.Join(seen, " ")
		mu.Unlock()
		if strings.Contains(got, "subagent.notice") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("OnEvent never got the notice: %s", got)
		}
	}
	if answer, err := a.Wake(context.Background()); err != nil || answer != "noted" {
		t.Fatalf("Wake: %q %v", answer, err)
	}
	var woken agent.SessionWoken
	for _, ev := range a.Events() {
		if ev.Type == agent.EvSessionWoken {
			_ = json.Unmarshal(ev.Payload, &woken)
		}
	}
	if woken.By != "caller" {
		t.Fatalf("woken: %+v", woken)
	}
}

// Wake with nothing waiting says so.
func TestSDKWakeNothingPending(t *testing.T) {
	a := bgAgent(t, bgModel(t, make(chan struct{}), ""), "notify", nil, nil)
	defer a.Close()
	if _, err := a.Wake(context.Background()); !errors.Is(err, abhed.ErrNothingToWake) {
		t.Fatalf("Wake: %v", err)
	}
}

// Close cancels the tasks still running, as the session closing.
func TestSDKCloseCancelsBackground(t *testing.T) {
	a := bgAgent(t, bgModel(t, make(chan struct{}), ""), "notify", nil, nil)
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if tasks := a.Background(); len(tasks) != 1 || tasks[0].Reason != string(agent.TermSessionClosed) {
		t.Fatalf("after Close: %+v", tasks)
	}
}

// Approve may be called after Run returned, for a background task's ask.
func TestSDKApproveCalledAfterRunReturned(t *testing.T) {
	asked := make(chan string, 1)
	a := bgAgent(t, bgModel(t, make(chan struct{}), "touch x.txt"), "notify",
		func(_ context.Context, tool string, _ json.RawMessage, _ abhed.Decision) (bool, error) {
			asked <- tool
			return false, nil
		}, nil)
	defer a.Close()
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	select {
	case tool := <-asked:
		if tool != "bash" {
			t.Fatalf("asked about %s", tool)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Approve was never called for the task's ask")
	}
}

// A Background that is not off, notify or auto is refused.
func TestSDKBackgroundUnknownRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := abhed.New(context.Background(), abhed.Options{Workspace: t.TempDir(), ConfiguredTools: true, Background: "sometimes",
		Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: "http://127.0.0.1:9", Model: "m", ContextWindow: 8192}})
	if err == nil {
		t.Fatal("Background sometimes was accepted")
	}
}

// eventLog collects the types and payloads OnEvent receives.
type eventLog struct {
	mu  sync.Mutex
	evs []abhed.Event
}

func (l *eventLog) add(ev abhed.Event) { l.mu.Lock(); l.evs = append(l.evs, ev); l.mu.Unlock() }

func (l *eventLog) has(typ agent.EventType, text string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ev := range l.evs {
		if ev.Type == typ && strings.Contains(string(ev.Payload), text) {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// In auto a result that arrives after Run returned starts a wake run with
// nobody asking: OnEvent gets session.woken naming the task, then the reply.
func TestSDKAutoWakesOnItsOwn(t *testing.T) {
	release := make(chan struct{})
	var log eventLog
	a := bgAgent(t, bgModel(t, release, ""), "auto", nil, log.add)
	defer a.Close()
	if answer, err := a.Run(context.Background(), "go"); err != nil || answer != "done" {
		t.Fatalf("Run: %q %v", answer, err)
	}
	id := a.Background()[0].ID
	close(release)
	waitFor(t, "the woken turn's reply", func() bool { return log.has(agent.EvAgentMessage, "noted") })
	if !log.has(agent.EvSessionWoken, `"by":"policy"`) || !log.has(agent.EvSessionWoken, id) {
		t.Fatal("the wake was not recorded as the session's own, naming the task")
	}
}

// A stop after the result arrived, before its wake, holds the wake.
func TestSDKAutoStopHoldsWake(t *testing.T) {
	release := make(chan struct{})
	var log eventLog
	a := bgAgent(t, bgModel(t, release, ""), "auto", nil, log.add)
	defer a.Close()
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.WaitBackground(ctx); err != nil {
		t.Fatal(err)
	}
	a.CancelTasks()
	waitFor(t, "the notice", func() bool { return log.has(agent.EvSubagentNotice, "skipped:stopped") })
	time.Sleep(200 * time.Millisecond)
	if log.has(agent.EvSessionWoken, "") {
		t.Fatal("a wake ran after the stop")
	}
}
