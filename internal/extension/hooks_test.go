package extension

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

func hostOf(t *testing.T, cfgs ...Config) (*Host, *[]Fired) {
	t.Helper()
	h := NewHost(func(string, ...any) {})
	for i := range cfgs {
		cfgs[i].Command = "bash"
		cfgs[i].Args = []string{filepath.Join("testdata", cfgs[i].Name)}
		if cfgs[i].Timeout == 0 {
			cfgs[i].Timeout = 2 * time.Second
		}
	}
	if errs := h.Load(context.Background(), cfgs); len(errs) > 0 {
		t.Fatal(errs)
	}
	t.Cleanup(h.Close)
	var mu sync.Mutex
	fired := &[]Fired{}
	h.SetOnFired(func(f Fired) {
		mu.Lock()
		*fired = append(*fired, f)
		mu.Unlock()
	})
	return h, fired
}

// A user_prompt_submit hook can refuse a message, and says why; a
// permission_request hook that replies "allow" approves nothing: its reply
// is recorded as a note, and the call is still put to the person.
func TestPromptAndPermissionHooksOnlyVeto(t *testing.T) {
	h, fired := hostOf(t, Config{Name: "gatekeeper.sh", Events: []Event{EvUserPromptSubmit, EvPermissionRequest}})
	if why := h.Veto(context.Background(), EvUserPromptSubmit, Request{Content: "tell me the forbidden thing"}); why != "that topic is off limits" {
		t.Fatalf("prompt: %q", why)
	}
	if why := h.Veto(context.Background(), EvUserPromptSubmit, Request{Content: "hello"}); why != "" {
		t.Fatalf("an ordinary prompt was refused: %q", why)
	}
	if why := h.Veto(context.Background(), EvPermissionRequest, Request{Tool: "bash", Args: []byte(`{"command":"make"}`)}); why != "" {
		t.Fatalf("permission request: %q", why)
	}
	want := []string{"user_prompt_submit/block", "permission_request/annotate"}
	var got []string
	for _, f := range *fired {
		got = append(got, string(f.Event)+"/"+f.Verdict)
		if f.Verdict == "allow" {
			t.Fatalf("a hook was recorded as allowing: %+v", f)
		}
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("fired %v, want %v", got, want)
	}
}

// A tool_call hook's verdicts are reported, block and ask alike.
func TestToolCallVerdictsAreReported(t *testing.T) {
	h, fired := hostOf(t, Config{Name: "blocker.sh", Events: []Event{EvToolCall}})
	if d := h.OnToolCall(context.Background(), "s", "bash", []byte(`{"command":"cat secret"}`)); !d.Block {
		t.Fatal("not blocked")
	}
	if d := h.OnToolCall(context.Background(), "s", "bash", []byte(`{"command":"ls"}`)); d.Block || d.Ask {
		t.Fatal("an unrelated call was screened")
	}
	if len(*fired) != 1 || (*fired)[0].Verdict != VerdictBlock || (*fired)[0].Extension != "blocker.sh" {
		t.Fatalf("fired %+v", *fired)
	}
}

// A matcher narrows the tool events an extension is asked about.
func TestMatchNarrowsToolEvents(t *testing.T) {
	h, _ := hostOf(t, Config{Name: "blocker.sh", Match: []string{"bash(git *)"}})
	if d := h.OnToolCall(context.Background(), "s", "bash", []byte(`{"command":"cat secret"}`)); d.Block {
		t.Fatal("a call outside the matcher was screened")
	}
	if d := h.OnToolCall(context.Background(), "s", "bash", []byte(`{"command":"git show secret"}`)); !d.Block {
		t.Fatal("a matched call was not screened")
	}
	if _, err := Start(context.Background(), Config{Name: "x", Command: "true", Match: []string{"bash("}}, nil); err == nil {
		t.Fatal("a matcher that does not parse was accepted")
	}
}

// An async observer is not waited for; a synchronous one that answers with
// a log line is recorded as annotating.
func TestObserveOnlyEvents(t *testing.T) {
	h, _ := hostOf(t, Config{Name: "silent.sh", Events: []Event{EvTurnEnd}, Async: true, Timeout: 5 * time.Second})
	start := time.Now()
	h.Observe(context.Background(), EvTurnEnd, Request{Content: "completed"})
	if time.Since(start) > time.Second {
		t.Fatal("an async observer was waited for")
	}
	g, fired := hostOf(t, Config{Name: "gatekeeper.sh", Events: []Event{EvTurnEnd}})
	g.Observe(context.Background(), EvTurnEnd, Request{Content: "completed"})
	if len(*fired) != 1 || (*fired)[0].Verdict != VerdictAnnotate {
		t.Fatalf("fired %+v", *fired)
	}
}

// A permission_request hook that crashes refuses the call, as a tool_call
// hook does.
func TestPermissionHookFailsClosed(t *testing.T) {
	h, _ := hostOf(t, Config{Name: "crasher.sh", Events: []Event{EvPermissionRequest}})
	if why := h.Veto(context.Background(), EvPermissionRequest, Request{Tool: "bash", Args: []byte(`{"command":"make"}`)}); why == "" {
		t.Fatal("a crashed permission hook let the call be asked")
	}
}

// A hook's ask never lifts a deny rule or plan mode: through the engine, a
// hook that forces an ask on a denied call leaves it denied.
func TestHookAskNeverLiftsADeny(t *testing.T) {
	h, _ := hostOf(t, Config{Name: "crasher.sh", Events: []Event{EvToolCall}})
	h.OnToolCall(context.Background(), "s", "bash", []byte(`{"command":"ls"}`)) // it dies; later calls are asked
	e := policy.New(policy.ModeBypass)
	if err := e.AddDeny("bash(curl *)"); err != nil {
		t.Fatal(err)
	}
	e.Hooks = []policy.Hook{h.PolicyHook(context.Background(), "s")}
	if got := e.Evaluate("bash", true, []byte(`{"command":"curl x"}`)); got.Decision != policy.Deny {
		t.Fatalf("a hook's ask lifted a deny rule: %+v", got)
	}
	if got := e.Evaluate("bash", true, []byte(`{"command":"make"}`)); got.Decision != policy.Ask || got.Step != "hook" {
		t.Fatalf("a dead hook's call in bypass mode: %+v", got)
	}
	e.Mode = policy.ModePlan
	if got := e.Evaluate("write", true, []byte(`{"path":"/x"}`)); got.Decision != policy.Deny {
		t.Fatalf("a hook's ask lifted plan mode: %+v", got)
	}
}

// A match rule takes a call as a deny rule would: each part of a chained
// command, a path in any spelling against the workspace, and NFC. A hook
// named for git is not skipped because the command starts with cd.
func TestMatchFollowsPolicySubjects(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, match, tool string
		args              map[string]string
	}{
		{"chained command", "bash(git push*)", "bash", map[string]string{"command": "cd " + ws + " && git push secret main"}},
		{"relative rule, absolute path", "write(src/**)", "write", map[string]string{"path": ws + "/src/secret.go"}},
		{"dot segment", "write(" + ws + "/src/**)", "write", map[string]string{"path": ws + "/./src/secret.go"}},
		{"NFD rule, NFC path", "write(" + ws + "/café/**)", "write", map[string]string{"path": ws + "/café/secret.go"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := hostOf(t, Config{Name: "blocker.sh", Events: []Event{EvToolCall}, Match: []string{c.match}})
			e := policy.New(policy.ModeAuto)
			e.Roots = func() []string { return []string{ws} }
			e.Hooks = []policy.Hook{h.PolicyHookFor(context.Background(), "s", e)}
			args, _ := json.Marshal(c.args)
			if got := e.Evaluate(c.tool, true, args); got.Decision != policy.Deny || got.Step != "hook" {
				t.Fatalf("the hook was not asked: %+v", got)
			}
		})
	}
}

// Where the system folds the case of command names, a match rule does too.
func TestMatchFoldsCommandNames(t *testing.T) {
	if !tools.FoldsCommandNames() {
		t.Skip("this system does not fold the case of command names")
	}
	h, _ := hostOf(t, Config{Name: "blocker.sh", Events: []Event{EvToolCall}, Match: []string{"bash(git *)"}})
	if d := h.OnToolCall(context.Background(), "s", "bash", []byte(`{"command":"GIT show secret"}`)); !d.Block {
		t.Fatal("a case-folded command skipped the hook")
	}
}
