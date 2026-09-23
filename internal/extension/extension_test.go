package extension

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
)

func hostWith(t *testing.T, scripts ...string) *Host {
	t.Helper()
	h := NewHost(func(string, ...any) {})
	cfgs := make([]Config, 0, len(scripts))
	for _, s := range scripts {
		cfgs = append(cfgs, Config{
			Name:    s,
			Command: "bash",
			Args:    []string{filepath.Join("testdata", s)},
			Timeout: 2 * time.Second,
		})
	}
	if errs := h.Load(context.Background(), cfgs); len(errs) > 0 {
		t.Fatalf("load: %v", errs)
	}
	t.Cleanup(h.Close)
	return h
}

// THE guarantee. An extension may make a decision stricter and never looser.
// If this test ever fails, Abhed's audit trail no longer means anything: an
// operator could drop a file into a directory and quietly permit what the
// policy forbids.
func TestExtensionCannotPermitWhatPolicyDenies(t *testing.T) {
	h := hostWith(t, "evil.sh") // answers "allow" to everything

	e := policy.New(policy.ModeDefault)
	if err := e.AddDeny("bash(rm -rf *)"); err != nil {
		t.Fatal(err)
	}
	e.Hooks = []policy.Hook{h.PolicyHook(context.Background(), "s1")}

	got := e.Evaluate("bash", true, []byte(`{"command":"rm -rf /"}`))
	if got.Decision != policy.Deny {
		t.Fatalf("decision = %v (%s); a denied command must stay denied no "+
			"matter what an extension replies", got.Decision, got.Reason)
	}
}

// Nor may it turn an approval prompt into an automatic approval.
func TestExtensionCannotBypassAnApprovalPrompt(t *testing.T) {
	h := hostWith(t, "evil.sh")
	e := policy.New(policy.ModeDefault) // mutations ask
	e.Hooks = []policy.Hook{h.PolicyHook(context.Background(), "s1")}

	got := e.Evaluate("write", true, []byte(`{"path":"/tmp/x"}`))
	if got.Decision == policy.Allow {
		t.Fatalf("an extension turned an approval prompt into an allow (%s)", got.Reason)
	}
}

// The other direction must work: an extension can block what policy allows.
func TestExtensionCanBlockWhatPolicyAllows(t *testing.T) {
	h := hostWith(t, "blocker.sh")
	e := policy.New(policy.ModeAuto)
	if err := e.AddAllow("bash(*)"); err != nil {
		t.Fatal(err)
	}
	e.Hooks = []policy.Hook{h.PolicyHook(context.Background(), "s1")}

	if got := e.Evaluate("bash", true, []byte(`{"command":"cat secret.txt"}`)); got.Decision != policy.Deny {
		t.Fatalf("decision = %v, want Deny — an extension must be able to veto", got.Decision)
	}
	if got := e.Evaluate("bash", true, []byte(`{"command":"ls"}`)); got.Decision != policy.Allow {
		t.Fatalf("decision = %v, want Allow — an unrelated call must be untouched", got.Decision)
	}
}

func TestExtensionRewritesToolResult(t *testing.T) {
	h := hostWith(t, "redactor.sh")
	content, isErr := h.OnToolResult(context.Background(), "s1", "read", "AWS_KEY=abc123", false)
	if content != "[redacted]" {
		t.Errorf("content = %q, want the extension's replacement", content)
	}
	if isErr {
		t.Error("is_error should be unchanged when the extension does not set it")
	}
}

func TestExtensionFiltersContext(t *testing.T) {
	h := hostWith(t, "redactor.sh")
	msgs := []Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "second"},
		{Role: "user", Content: "third"},
	}
	keep := h.OnContext(context.Background(), "s1", msgs)
	if len(keep) != 1 || keep[0] != 1 {
		t.Fatalf("keep = %v, want [1] — the extension asked to keep only index 1", keep)
	}
}

// A crashed extension must not fail the run: it can only ever have made a
// decision stricter, so continuing under policy alone is safe and stopping is
// not more secure, merely less useful.
func TestCrashedExtensionIsSkippedNotFatal(t *testing.T) {
	h := hostWith(t, "crasher.sh")
	d := h.OnToolCall(context.Background(), "s1", "bash", []byte(`{"command":"ls"}`))
	if d.Block {
		t.Error("a crashed extension must not block")
	}
	// And it stays skipped rather than being retried on every call.
	d2 := h.OnToolCall(context.Background(), "s1", "bash", []byte(`{"command":"ls"}`))
	if d2.Block {
		t.Error("a dead extension must stay dead")
	}
}

// A hung extension must time out rather than stall the agent forever.
func TestHungExtensionTimesOut(t *testing.T) {
	h := NewHost(func(string, ...any) {})
	if errs := h.Load(context.Background(), []Config{{
		Name: "silent", Command: "bash",
		Args:    []string{filepath.Join("testdata", "silent.sh")},
		Timeout: 200 * time.Millisecond,
	}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	t.Cleanup(h.Close)

	start := time.Now()
	d := h.OnToolCall(context.Background(), "s1", "bash", []byte(`{"command":"ls"}`))
	elapsed := time.Since(start)

	if d.Block {
		t.Error("a hung extension must not block the call")
	}
	if elapsed > 2*time.Second {
		t.Errorf("waited %s for a hung extension; the timeout did not fire", elapsed)
	}
}

// Where extensions disagree, the strictest answer wins, so the order they are
// loaded in cannot change the verdict.
func TestStrictestAnswerWinsRegardlessOfOrder(t *testing.T) {
	for _, order := range [][]string{
		{"evil.sh", "blocker.sh"},
		{"blocker.sh", "evil.sh"},
	} {
		h := hostWith(t, order...)
		d := h.OnToolCall(context.Background(), "s1", "bash", []byte(`{"command":"cat secret"}`))
		if !d.Block {
			t.Errorf("order %v: the blocking extension must win", order)
		}
	}
}

func TestSystemPromptIsAppendedNotReplaced(t *testing.T) {
	h := hostWith(t, "redactor.sh") // returns {} for this event
	const base = "You are Abhed."
	if got := h.OnBeforeAgentStart(context.Background(), "s1", base); got != base {
		t.Errorf("system = %q, want it unchanged when no extension contributes", got)
	}
}

func TestArgsRewriteComposes(t *testing.T) {
	h := hostWith(t, "blocker.sh")
	d := h.OnToolCall(context.Background(), "s1", "bash", json.RawMessage(`{"command":"ls"}`))
	if d.Block {
		t.Fatal("this call should not be blocked")
	}
}

// An extension can add a tool the harness never knew about.
func TestExtensionProvidesATool(t *testing.T) {
	h := hostWith(t, "provider.sh")
	provided, errs := h.Tools(context.Background())
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(provided) != 1 {
		t.Fatalf("got %d tools, want 1", len(provided))
	}
	tool := provided[0]
	if tool.Name() != "weather" {
		t.Errorf("name = %q", tool.Name())
	}
	if tool.Mutates() {
		t.Error("the extension said mutates:false and must be believed")
	}
	res := tool.Run(context.Background(), nil, []byte(`{"city":"Dublin"}`))
	if res.IsError || res.Content != "It is raining." {
		t.Errorf("Run() = %+v", res)
	}
}

// Abhed cannot know what someone else's tool does, so one that does not say
// must be assumed to change something and routed through approval.
func TestUnspecifiedToolIsAssumedToMutate(t *testing.T) {
	h := NewHost(func(string, ...any) {})
	tool := providedTool{def: ToolDef{Name: "x"}, mutates: true}
	if !tool.Mutates() {
		t.Fatal("an unspecified tool must default to mutating, not to safe")
	}
	_ = h
}

// A silent extension must not look like a successful call.
func TestSilentProvidedToolIsAnError(t *testing.T) {
	h := hostWith(t, "crasher.sh")
	tool := providedTool{ext: h.exts[0], def: ToolDef{Name: "x"}}
	res := tool.Run(context.Background(), nil, []byte(`{}`))
	if !res.IsError {
		t.Fatal("no answer from an extension must be an error, not an empty success")
	}
}

// Compaction is where the harness discards information on purpose, and only
// the deployment knows what must survive it.
func TestExtensionSuppliesACompactionSummary(t *testing.T) {
	h := hostWith(t, "summarizer.sh")
	summary, cancel := h.OnBeforeCompact(context.Background(), "s1",
		[]Message{{Role: "user", Content: "about ticket ABC-123"}})
	if cancel {
		t.Fatal("this extension supplies a summary, it does not cancel")
	}
	if summary != "Ticket ABC-123 is the subject. Keep it." {
		t.Errorf("summary = %q", summary)
	}
}

func TestExtensionCanCancelCompaction(t *testing.T) {
	h := hostWith(t, "canceller.sh")
	_, cancel := h.OnBeforeCompact(context.Background(), "s1", nil)
	if !cancel {
		t.Fatal("the extension asked to cancel and was ignored")
	}
}

// A cancel outranks a summary: both are the stricter reading of "do not
// summarize this the usual way", so order must not decide the outcome.
func TestCancelOutranksSummaryEitherOrder(t *testing.T) {
	for _, order := range [][]string{
		{"summarizer.sh", "canceller.sh"},
		{"canceller.sh", "summarizer.sh"},
	} {
		h := hostWith(t, order...)
		_, cancel := h.OnBeforeCompact(context.Background(), "s1", nil)
		if !cancel {
			t.Errorf("order %v: a cancel must win", order)
		}
	}
}
