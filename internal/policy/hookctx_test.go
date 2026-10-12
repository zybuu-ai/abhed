package policy

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// A hook that has not decided by the deadline refuses the call, and once a
// batch's deadline has passed, no further hook is called at all.
func TestHooksAreBoundByADeadline(t *testing.T) {
	var calls atomic.Int32
	e := New(ModeBypass)
	e.HookTimeout = 50 * time.Millisecond
	e.ContextHooks = append(e.ContextHooks, func(*Engine) ContextHook {
		return func(ctx context.Context, _ string, _ json.RawMessage) *Result {
			calls.Add(1)
			<-ctx.Done()
			return nil
		}
	})
	start := time.Now()
	res := e.Evaluate("write", true, args(map[string]string{"path": "a"}))
	if res.Decision != Deny || res.Step != "hook" || time.Since(start) > 5*time.Second {
		t.Fatalf("= %s at %s (%s) after %s", res.Decision, res.Step, res.Reason, time.Since(start))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done()
	calls.Store(0)
	for range 1000 {
		if res := e.EvaluateContext(ctx, "write", true, args(map[string]string{"path": "a"})); res.Decision != Deny {
			t.Fatalf("after the deadline: %s (%s)", res.Decision, res.Reason)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d hooks were called after the deadline", n)
	}
	// A plain hook slower than the bound is refused too.
	e = New(ModeBypass)
	e.HookTimeout = 20 * time.Millisecond
	e.Hooks = append(e.Hooks, func(string, json.RawMessage) *Result { time.Sleep(60 * time.Millisecond); return nil })
	if res := e.Evaluate("write", true, args(map[string]string{"path": "a"})); res.Decision != Deny {
		t.Fatalf("slow plain hook: %s (%s)", res.Decision, res.Reason)
	}
	if !e.Screens("bash") {
		t.Fatal("an engine with hooks does not screen")
	}
}

// A dry run asks no hook of any kind, and leaves the engine's own hooks in place.
func TestWithoutHooksAsksNoHook(t *testing.T) {
	asked := ""
	e := New(ModeDefault)
	e.Hooks = []Hook{func(string, json.RawMessage) *Result { asked += "hook "; return nil }}
	e.EngineHooks = []func(*Engine) Hook{func(*Engine) Hook {
		return func(string, json.RawMessage) *Result { asked += "engine "; return nil }
	}}
	e.ContextHooks = []func(*Engine) ContextHook{func(*Engine) ContextHook {
		return func(context.Context, string, json.RawMessage) *Result { asked += "context "; return nil }
	}}
	e.WithoutHooks().Evaluate("write", true, args(map[string]string{"path": "a"}))
	if asked != "" {
		t.Fatalf("a dry run asked: %s", asked)
	}
	e.Evaluate("write", true, args(map[string]string{"path": "a"}))
	if asked != "hook engine context " {
		t.Fatalf("the engine's own hooks were changed: %q", asked)
	}
}
