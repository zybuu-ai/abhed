package extension

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// scriptRunner answers each command with the result it names, and keeps the
// stdin each was given.
type scriptRunner struct {
	mu      sync.Mutex
	results map[string]CommandResult
	stdin   map[string][]byte
	dirs    map[string]string
}

func (s *scriptRunner) run(ctx context.Context, dir, command string, stdin []byte) (CommandResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin == nil {
		s.stdin, s.dirs = map[string][]byte{}, map[string]string{}
	}
	s.stdin[command], s.dirs[command] = stdin, dir
	if command == "hang" {
		<-ctx.Done()
		return CommandResult{Exit: -1}, ctx.Err()
	}
	r, ok := s.results[command]
	if !ok {
		return CommandResult{}, errors.New("no such command")
	}
	return r, nil
}

func hooksOf(t *testing.T, env HookEnv, cfgs ...HookConfig) (*Host, *[]Fired) {
	t.Helper()
	h := NewHost(nil)
	if errs := h.LoadHooks(cfgs, env); len(errs) > 0 {
		t.Fatal(errs)
	}
	var mu sync.Mutex
	fired := &[]Fired{}
	h.SetOnFired(func(f Fired) {
		mu.Lock()
		*fired = append(*fired, f)
		mu.Unlock()
	})
	return h, fired
}

// Exit 0 goes on, exit 2 blocks with stderr as the reason, and any other
// exit, or a hook that runs past its timeout, refuses the call: a hook that
// could not decide is not one that agreed.
func TestCommandHookExitCodes(t *testing.T) {
	r := &scriptRunner{results: map[string]CommandResult{
		"ok":      {Exit: 0, Stdout: []byte("all fine\n")},
		"block":   {Exit: 2, Stderr: []byte("no pushes to main\n")},
		"crash":   {Exit: 1, Stderr: []byte("oops")},
		"garbage": {Exit: 0, Stdout: []byte("{not json")},
	}}
	for cmd, want := range map[string]string{
		"ok":      "",
		"block":   "no pushes to main",
		"crash":   "hook crash did not answer (exited with status 1: oops), so the call is refused",
		"garbage": "hook garbage did not answer",
		"hang":    "did not finish within",
	} {
		h, fired := hooksOf(t, HookEnv{Run: r.run}, HookConfig{Name: cmd, Command: cmd, Events: []Event{EvToolCall}, Timeout: 200 * time.Millisecond})
		d := h.OnToolCall(context.Background(), "s1", "bash", json.RawMessage(`{"command":"git push"}`))
		if want == "" {
			if d.Block || d.Ask {
				t.Fatalf("%s: %+v", cmd, d)
			}
		} else if !d.Block || !strings.Contains(d.Reason, want) {
			t.Fatalf("%s: got %+v, want a block saying %q", cmd, d, want)
		}
		// Each run is recorded once, by the hook, with its exit status.
		if len(*fired) != 1 || (*fired)[0].Kind != KindCommand || (*fired)[0].SessionID != "s1" {
			t.Fatalf("%s: recorded %+v", cmd, *fired)
		}
		f := (*fired)[0]
		switch cmd {
		case "ok":
			if f.Verdict != VerdictNone || f.ExitCode == nil || *f.ExitCode != 0 {
				t.Fatalf("ok recorded %+v", f)
			}
		case "block":
			if f.Verdict != VerdictBlock || *f.ExitCode != 2 || f.Reason != "no pushes to main" {
				t.Fatalf("block recorded %+v", f)
			}
		default:
			if f.Verdict != VerdictError {
				t.Fatalf("%s recorded %+v", cmd, f)
			}
		}
	}
	var req Request
	if err := json.Unmarshal(r.stdin["ok"], &req); err != nil || req.Event != EvToolCall || req.Tool != "bash" || req.SessionID != "s1" {
		t.Fatalf("the event on stdin was %s (%v)", r.stdin["ok"], err)
	}
}

// A simple hook only tightens: whatever it prints, it never makes a policy
// decision Allow, never rewrites a call, and a permission_request hook that
// says "approve" leaves the call to be asked.
func TestCommandHookNeverApproves(t *testing.T) {
	r := &scriptRunner{results: map[string]CommandResult{
		"yes": {Exit: 0, Stdout: []byte(`{"decision":"approve","allow":true,"args":{"command":"rm -rf /"},"block":false}`)},
	}}
	h, _ := hooksOf(t, HookEnv{Run: r.run}, HookConfig{Name: "yes", Command: "yes", Events: []Event{EvToolCall, EvPermissionRequest}})
	d := h.OnToolCall(context.Background(), "s", "bash", json.RawMessage(`{"command":"ls"}`))
	if d.Block || d.Ask || d.Args != nil {
		t.Fatalf("decision %+v", d)
	}
	if res := h.ContextPolicyHookFor("s", nil)(context.Background(), "bash", json.RawMessage(`{"command":"ls"}`)); res != nil {
		t.Fatalf("policy hook returned %+v", res)
	}
	if why := h.Veto(context.Background(), EvPermissionRequest, Request{Tool: "bash", Args: json.RawMessage(`{}`)}); why != "" {
		t.Fatalf("veto %q", why)
	}
	// Under a deny rule the engine's answer stands.
	p := policy.New(policy.ModeAuto)
	if err := p.AddDeny("bash(ls*)"); err != nil {
		t.Fatal(err)
	}
	p.ContextHooks = append(p.ContextHooks, func(e *policy.Engine) policy.ContextHook { return h.ContextPolicyHookFor("s", e) })
	if got := p.EvaluateContext(context.Background(), "bash", true, json.RawMessage(`{"command":"ls"}`)); got.Decision != policy.Deny {
		t.Fatalf("a hook's yes undid a deny rule: %+v", got)
	}
}

// Match narrows a hook to the calls its rules take, as an extension's do.
func TestCommandHookMatch(t *testing.T) {
	r := &scriptRunner{results: map[string]CommandResult{"no": {Exit: 2, Stderr: []byte("not git")}}}
	h, fired := hooksOf(t, HookEnv{Run: r.run}, HookConfig{Name: "git", Command: "no", Events: []Event{EvToolCall}, Match: []string{"bash(git *)"}})
	if d := h.OnToolCall(context.Background(), "s", "bash", json.RawMessage(`{"command":"make"}`)); d.Block || len(*fired) != 0 {
		t.Fatalf("an unmatched call reached the hook: %+v %v", d, *fired)
	}
	if d := h.OnToolCall(context.Background(), "s", "bash", json.RawMessage(`{"command":"git push"}`)); !d.Block {
		t.Fatalf("a matched call was not blocked: %+v", d)
	}
}

// On stop, exit 2 asks the agent to continue with stderr as the instruction;
// a stop hook that fails lets the run end.
func TestStopHook(t *testing.T) {
	r := &scriptRunner{results: map[string]CommandResult{
		"again": {Exit: 2, Stderr: []byte("the tests have not run")},
		"fine":  {Exit: 0},
		"crash": {Exit: 1},
	}}
	h, fired := hooksOf(t, HookEnv{Run: r.run},
		HookConfig{Name: "crash", Command: "crash", Events: []Event{EvStop}},
		HookConfig{Name: "fine", Command: "fine", Events: []Event{EvStop}},
		HookConfig{Name: "again", Command: "again", Events: []Event{EvStop}})
	name, why := h.OnStop(context.Background(), "s", "all done")
	if name != "again" || why != "the tests have not run" {
		t.Fatalf("stop: %q %q", name, why)
	}
	var req Request
	if err := json.Unmarshal(r.stdin["again"], &req); err != nil || req.Event != EvStop || req.Content != "all done" {
		t.Fatalf("stdin %s", r.stdin["again"])
	}
	var verdicts []string
	for _, f := range *fired {
		verdicts = append(verdicts, f.Extension+"/"+f.Verdict)
	}
	if strings.Join(verdicts, " ") != "crash/error fine/none again/continue" {
		t.Fatalf("recorded %v", verdicts)
	}
}

// An HTTP hook is POSTed the event; a 2xx with a block blocks, and an answer
// that is not 2xx, or a client that refuses the request, refuses the call.
func TestHTTPHook(t *testing.T) {
	var got Request
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		header = r.Header.Get("X-Token")
		switch r.URL.Path {
		case "/block":
			_, _ = w.Write([]byte(`{"block":true,"reason":"change freeze"}`))
		case "/ok":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "no", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	for path, want := range map[string]string{"/block": "change freeze", "/ok": "", "/fail": "answered 500"} {
		h, fired := hooksOf(t, HookEnv{HTTP: srv.Client()},
			HookConfig{Name: "web", URL: srv.URL + path, Headers: map[string]string{"X-Token": "t1"}, Events: []Event{EvToolCall}})
		d := h.OnToolCall(context.Background(), "s9", "write", json.RawMessage(`{"path":"a"}`))
		if want == "" && (d.Block || d.Ask) || want != "" && (!d.Block || !strings.Contains(d.Reason, want)) {
			t.Fatalf("%s: %+v", path, d)
		}
		if got.Tool != "write" || got.SessionID != "s9" || header != "t1" {
			t.Fatalf("%s: sent %+v with header %q", path, got, header)
		}
		if len(*fired) != 1 || (*fired)[0].Kind != KindHTTP || (*fired)[0].ExitCode != nil {
			t.Fatalf("%s: recorded %+v", path, *fired)
		}
	}
	// A client that refuses the request, as the egress guard does, refuses the call.
	refusing := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("egress: not on the allowlist")
	})}
	h, _ := hooksOf(t, HookEnv{HTTP: refusing}, HookConfig{Name: "web", URL: srv.URL + "/ok", Events: []Event{EvToolCall}})
	if d := h.OnToolCall(context.Background(), "s", "bash", json.RawMessage(`{}`)); !d.Block || !strings.Contains(d.Reason, "allowlist") {
		t.Fatalf("a refused request let the call through: %+v", d)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A hook takes only the events a hook can act on, and needs one of command and url.
func TestLoadHooksRefusesBadConfig(t *testing.T) {
	h := NewHost(nil)
	errs := h.LoadHooks([]HookConfig{
		{Name: "a", Command: "x", Events: []Event{EvListTools}},
		{Name: "b", Command: "x", URL: "http://x", Events: []Event{EvToolCall}},
		{Name: "c", Command: "x"},
		{Name: "d", Command: "x", Events: []Event{EvToolCall}, Match: []string{"bash(("}},
		{Name: "ok", Command: "x", Events: []Event{EvTurnEnd}},
	}, HookEnv{})
	if len(errs) != 4 || h.Len() != 1 {
		t.Fatalf("errors %v, loaded %d", errs, h.Len())
	}
}

// The reply shapes hooks written for other harnesses print mean the same
// here: decision block blocks, and on stop sends the agent on; continue
// false ends the run; continue true is no opinion, and only stop continues.
func TestHookReplyShapes(t *testing.T) {
	r := &scriptRunner{results: map[string]CommandResult{
		"decide": {Exit: 0, Stdout: []byte(`{"decision":"block","reason":"not on main"}`)},
		"carry":  {Exit: 0, Stdout: []byte(`{"continue":true}`)},
		"halt":   {Exit: 0, Stdout: []byte(`{"continue":false,"stopReason":"change freeze"}`)},
	}}
	ctx := WithSession(context.Background(), "s7", "/work/tree")
	h, fired := hooksOf(t, HookEnv{Run: r.run},
		HookConfig{Name: "carry", Command: "carry", Events: []Event{EvToolCall, EvStop}},
		HookConfig{Name: "decide", Command: "decide", Events: []Event{EvToolCall, EvStop}, Match: []string{"bash(git push*)"}},
		HookConfig{Name: "halt", Command: "halt", Events: []Event{EvToolCall}, Match: []string{"bash(deploy*)"}})
	if d := h.OnToolCall(ctx, "s7", "bash", json.RawMessage(`{"command":"git push"}`)); !d.Block || d.Reason != "not on main" {
		t.Fatalf("decision block: %+v", d)
	}
	if d := h.OnToolCall(ctx, "s7", "bash", json.RawMessage(`{"command":"ls"}`)); d.Block || d.Ask {
		t.Fatalf("continue true: %+v", d)
	}
	if why := h.Halted("s7"); why != "" {
		t.Fatalf("halted early: %q", why)
	}
	if d := h.OnToolCall(ctx, "s7", "bash", json.RawMessage(`{"command":"deploy prod"}`)); !d.Block {
		t.Fatalf("a call a hook ended the run on went ahead: %+v", d)
	}
	if why := h.Halted("s7"); why != "change freeze" {
		t.Fatalf("halted: %q", why)
	}
	if why := h.Halted("s7"); why != "" {
		t.Fatalf("a halt was taken twice: %q", why)
	}
	if name, why := h.OnStop(ctx, "s7", "done"); name != "decide" || why != "not on main" {
		t.Fatalf("stop: %q %q", name, why)
	}
	for _, f := range *fired {
		if f.Verdict == VerdictContinue && f.Event != EvStop {
			t.Fatalf("a %s hook was recorded as continuing: %+v", f.Event, f)
		}
	}
	if r.dirs["carry"] != "/work/tree" {
		t.Fatalf("the hook ran in %q", r.dirs["carry"])
	}
}

// The session a decision is for, where its context says, is the one the
// hook is told, not the one the policy hook was made with.
func TestPolicyHookTellsTheSession(t *testing.T) {
	r := &scriptRunner{results: map[string]CommandResult{"ok": {Exit: 0}}}
	h, _ := hooksOf(t, HookEnv{Run: r.run}, HookConfig{Name: "ok", Command: "ok", Events: []Event{EvToolCall}})
	h.ContextPolicyHookFor("session", nil)(WithSession(context.Background(), "real-1", ""), "bash", json.RawMessage(`{}`))
	var req Request
	if err := json.Unmarshal(r.stdin["ok"], &req); err != nil || req.SessionID != "real-1" {
		t.Fatalf("sent %s", r.stdin["ok"])
	}
}

// A hook on an event that only observes is bounded by a short timeout unless
// it sets one, and an async one is not waited for.
func TestObserveHookTimeouts(t *testing.T) {
	r := &scriptRunner{}
	h, fired := hooksOf(t, HookEnv{Run: r.run}, HookConfig{Name: "slow", Command: "hang", Events: []Event{EvTurnEnd}})
	start := time.Now()
	h.Observe(context.Background(), EvTurnEnd, Request{SessionID: "s"})
	if took := time.Since(start); took > 3*time.Second || len(*fired) != 1 || (*fired)[0].Verdict != VerdictError {
		t.Fatalf("took %s, recorded %+v", took, *fired)
	}
	h, _ = hooksOf(t, HookEnv{Run: r.run}, HookConfig{Name: "bg", Command: "hang", Events: []Event{EvTurnEnd}, Async: true})
	start = time.Now()
	h.Observe(context.Background(), EvTurnEnd, Request{SessionID: "s"})
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("an async hook was waited for: %s", took)
	}
}

// headers_env sends a variable's value; one that is not set fails the hook.
func TestHTTPHookHeadersEnv(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("HOOK_TOKEN_TEST", "Bearer t-1")
	h, _ := hooksOf(t, HookEnv{HTTP: srv.Client()}, HookConfig{Name: "web", URL: srv.URL,
		HeadersEnv: map[string]string{"Authorization": "HOOK_TOKEN_TEST"}, Events: []Event{EvToolCall}})
	if d := h.OnToolCall(context.Background(), "s", "bash", json.RawMessage(`{}`)); d.Block || got != "Bearer t-1" {
		t.Fatalf("%+v, sent %q", d, got)
	}
	h, _ = hooksOf(t, HookEnv{HTTP: srv.Client()}, HookConfig{Name: "web", URL: srv.URL,
		HeadersEnv: map[string]string{"Authorization": "HOOK_TOKEN_UNSET_TEST"}, Events: []Event{EvToolCall}})
	if d := h.OnToolCall(context.Background(), "s", "bash", json.RawMessage(`{}`)); !d.Block || !strings.Contains(d.Reason, "HOOK_TOKEN_UNSET_TEST is not set") {
		t.Fatalf("%+v", d)
	}
}
