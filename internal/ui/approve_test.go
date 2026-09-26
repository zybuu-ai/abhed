package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

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
		{"a\n", true},
		{"y\n", true},
		{"\n", false},   // Enter alone never accepts; input then ends
		{"\na\n", true}, // Enter re-prompts, then an explicit key accepts
		{"r\n", false},
		{"n\n", false},
		{"x\na\n", true}, // an unknown key re-prompts, then accept
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

// TestApproveSingleKey covers the interactive path Prepare installs: one
// keypress decides, with no Enter, and the thinking indicator is paused and
// resumed exactly once around the prompt.
func TestApproveSingleKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"a", true},
		{"y", true},
		{"r", false},
		{"n", false},
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
	answers := []string{"A"}
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

// The editor's notices, and Enter alone, re-show the choices without deciding.
func TestApproveHeldNoticeDoesNotDecide(t *testing.T) {
	var out strings.Builder
	a := NewApprover(&out)
	answers := []string{string(approvalHeld), string(approvalBusy), "\r", "r"}
	a.Prepare = func(ctx context.Context) (func() (string, bool), func()) {
		return func() (string, bool) {
			s := answers[0]
			answers = answers[1:]
			return s, true
		}, func() {}
	}
	got, err := a.Approve(context.Background(), "write", json.RawMessage(`{}`), policy.Result{})
	if err != nil || got {
		t.Fatalf("got %v err %v, want a rejection from the explicit key", got, err)
	}
	for _, want := range []string{"steering message", "the line is not empty"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("notice %q was not shown:\n%s", want, out.String())
		}
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
