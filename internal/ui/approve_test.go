package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// newTestApprover builds an approver writing to a discarded buffer, so tests
// assert the decision rather than the exact prompt text.
func newTestApprover(in io.Reader) *Approver {
	a := NewApprover(io.Discard)
	a.In = in
	return a
}

// TestApproveLineInput covers the non-interactive path: a decision arrives as a
// newline-terminated line, which is how a piped session or a test drives it.
func TestApproveLineInput(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"1\n", true},
		{"2\n", false},   // with no scope offered, 2 is No
		{"\n", false},    // Enter alone never accepts; input then ends
		{"\n1\n", true},  // Enter re-prompts, then a number accepts
		{"x\n1\n", true}, // anything else re-prompts, then a number
		{"3\n", false},   // not offered: re-prompts, then input ends
	}
	// A letter never approves, in line mode as in the dialog: every one is
	// asked again, and input ending refuses.
	for _, letter := range []string{"y", "a", "A", "Y", "yes", "ok"} {
		cases = append(cases, struct {
			in   string
			want bool
		}{letter + "\n", false})
	}
	for _, c := range cases {
		a := newTestApprover(strings.NewReader(c.in))
		got, err := a.Approve(context.Background(), "bash", json.RawMessage(`{"command":"ls"}`), policy.Result{})
		if err != nil {
			t.Fatalf("in %q: unexpected error %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("in %q: got %v, want %v", c.in, got, c.want)
		}
	}
}

// TestApproveEOFRefuses ensures that input ending without a decision refuses
// rather than proceeding — the safe default for a piped run with no answer.
func TestApproveEOFRefuses(t *testing.T) {
	a := newTestApprover(strings.NewReader(""))
	got, err := a.Approve(context.Background(), "bash", json.RawMessage(`{}`), policy.Result{})
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if got {
		t.Errorf("EOF should refuse, got accept")
	}
}

// TestApproveSingleKey covers the path Prepare installs: the answer is read
// through it, and the thinking indicator is paused and resumed exactly once
// around the prompt.
func TestApproveSingleKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"1", true},
		{"2", false},
	}
	for _, c := range cases {
		a := NewApprover(io.Discard)
		paused, resumed := 0, 0
		a.Prepare = func(ctx context.Context) (func() (string, bool), func()) {
			paused++
			read := func() (string, bool) { return c.key, true }
			return read, func() { resumed++ }
		}
		got, err := a.Approve(context.Background(), "bash", json.RawMessage(`{}`), policy.Result{})
		if err != nil {
			t.Fatalf("key %q: unexpected error %v", c.key, err)
		}
		if got != c.want {
			t.Errorf("key %q: got %v, want %v", c.key, got, c.want)
		}
		if paused != 1 || resumed != 1 {
			t.Errorf("key %q: prepare/cleanup ran %d/%d times, want 1/1", c.key, paused, resumed)
		}
	}
}

// TestApproveAlwaysAllow covers the [A]lways branch: it accepts and remembers
// the scope so the next call with the same scope is silent.
func TestApproveAlwaysAllow(t *testing.T) {
	a := NewApprover(io.Discard)
	answers := []string{"2"}
	a.Prepare = func(ctx context.Context) (func() (string, bool), func()) {
		return func() (string, bool) {
			if len(answers) == 0 {
				return "", false
			}
			s := answers[0]
			answers = answers[1:]
			return s, true
		}, func() {}
	}
	res := policy.Result{Decision: policy.Ask, Step: "default", Scope: "bash(ls*)"}
	got, err := a.Approve(context.Background(), "bash", json.RawMessage(`{}`), res)
	if err != nil || !got {
		t.Fatalf("always-allow should accept: got %v err %v", got, err)
	}
	if !a.Session.Has("bash(ls*)") {
		t.Fatalf("scope was not remembered")
	}
	// A second call with the same scope must not consult input at all.
	a.Prepare = func(ctx context.Context) (func() (string, bool), func()) {
		return func() (string, bool) {
			t.Fatal("input should not be read for an already-allowed scope")
			return "", false
		}, func() {}
	}
	got, err = a.Approve(context.Background(), "bash", json.RawMessage(`{}`), res)
	if err != nil || !got {
		t.Fatalf("remembered scope should auto-accept: got %v err %v", got, err)
	}
}

// Enter alone and letters re-show the choices without deciding; the
// choices are numbered.
func TestApproveAsksAgainUntilANumber(t *testing.T) {
	var out strings.Builder
	a := NewApprover(&out)
	answers := []string{"\r", "y", "a", "3"}
	a.Prepare = func(ctx context.Context) (func() (string, bool), func()) {
		return func() (string, bool) {
			s := answers[0]
			answers = answers[1:]
			return s, true
		}, func() {}
	}
	res := policy.Result{Decision: policy.Ask, Step: "default", Scope: "write(/ws/x)"}
	got, err := a.Approve(context.Background(), "write", json.RawMessage(`{}`), res)
	if err != nil || got {
		t.Fatalf("got %v err %v, want a refusal from 3", got, err)
	}
	for _, want := range []string{"1. Yes", "2. Yes, and don't ask again for write(/ws/x) this session", "3. No", "answer with a number"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
}

// A destructive command needs a second numbered yes; Enter and letters do
// not give it.
func TestApproveDestructiveAsksTwice(t *testing.T) {
	for in, want := range map[string]bool{"1\n2\n": true, "1\n1\n": false, "1\ny\n\n": false} {
		a := newTestApprover(strings.NewReader(in))
		got, _ := a.Approve(context.Background(), "bash", json.RawMessage(`{"command":"rm -rf build"}`),
			policy.Result{Decision: policy.Ask, Step: "destructive", Reason: "rm -rf"})
		if got != want {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}

// What the line prompt shows is filtered: the command is revealed, not
// drawn raw, so a carriage return cannot hide its head.
func TestApproveLineRevealsTheCommand(t *testing.T) {
	var out strings.Builder
	a := NewApprover(&out)
	a.In = strings.NewReader("3\n")
	args, _ := json.Marshal(map[string]string{"command": "touch pwned #\u200d\r│ $ ls -la \x1b]52;c;eA==\x07"})
	_, _ = a.Approve(context.Background(), "bash", args, policy.Result{Decision: policy.Ask, Reason: "r\x1b]0;T\x07"})
	got := out.String()
	for _, bad := range []string{"\r", "\x1b]", "\x07", "\u200d"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q reached the prompt: %q", bad, got)
		}
	}
	if !strings.Contains(got, "touch pwned #⟨U+200D⟩⟨\\r⟩│ $ ls -la") {
		t.Fatalf("the command is not shown whole: %q", got)
	}
}

// Ctrl-C cancels the turn's context while the prompt waits: the call is
// refused with the cancellation, which the loop records as an interrupt.
func TestApproveInterruptedRefuses(t *testing.T) {
	a := NewApprover(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	a.Prepare = func(ctx context.Context) (func() (string, bool), func()) {
		return func() (string, bool) {
			cancel()
			<-ctx.Done()
			return "", false
		}, func() {}
	}
	got, err := a.Approve(ctx, "write", json.RawMessage(`{}`), policy.Result{})
	if got || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v err %v, want a refusal with context.Canceled", got, err)
	}
}

// A subagent's ask names the subagent, so a person knows where it comes from.
func TestApproveNamesTheSubagent(t *testing.T) {
	var out strings.Builder
	a := NewApprover(&out)
	a.In = strings.NewReader("2\n")
	ctx := agent.WithSubagent(context.Background(), "audit pkg/auth")
	if _, err := a.Approve(ctx, "bash", json.RawMessage(`{"command":"rm -rf x"}`), policy.Result{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "asked by subagent: audit pkg/auth") {
		t.Fatalf("the prompt does not name the subagent: %q", out.String())
	}
}

// Only a line that is exactly a decision key may answer an approval.
func TestDecisionKeysOnly(t *testing.T) {
	for _, l := range []string{"a", "y", "r", "n", "A", " a \n"} {
		if !Decision(l) {
			t.Errorf("%q is a decision key", l)
		}
	}
	for _, l := range []string{"", "yes", "ok", "no", "approve", "a please", "R"} {
		if Decision(l) {
			t.Errorf("%q was taken as a decision", l)
		}
	}
	p := NewPrompter()
	if p.Waiting() {
		t.Fatal("waiting with no approval")
	}
	got := make(chan string, 1)
	go func() { s, _ := p.Await(context.Background()); got <- s }()
	for !p.Waiting() {
		time.Sleep(time.Millisecond)
	}
	if !p.Deliver("a") || <-got != "a" || p.Waiting() {
		t.Fatal("a waiting approval did not take its answer")
	}
}

// An answer says which ask it answered: the tool, what it acts on, and the
// subagent that asked. A piped key answers by position, so this is the line
// that shows what it approved.
func TestAnswerNamesTheAsk(t *testing.T) {
	var out strings.Builder
	a := NewApprover(&out)
	a.In = strings.NewReader("y\n")
	ctx := agent.WithSubagent(context.Background(), "scan logs")
	ok, err := a.Approve(ctx, "bash", json.RawMessage(`{"command":"touch made.txt"}`), policy.Result{Decision: policy.Ask})
	if err != nil || !ok {
		t.Fatalf("approve: %v %v", ok, err)
	}
	if got := out.String(); !strings.Contains(got, "accepted: bash") || !strings.Contains(got, "touch made.txt") || !strings.Contains(got, "(subagent scan logs)") {
		t.Fatalf("the answer does not name its ask:\n%s", got)
	}
}
