package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

type approverFunc func(ctx context.Context, res policy.Result) (bool, error)

func (f approverFunc) Approve(ctx context.Context, _ string, _ json.RawMessage, res policy.Result) (bool, error) {
	return f(ctx, res)
}

// writeTurns asks for one write, which default mode puts to the approver.
func writeTurns(dir string) []scriptedTurn {
	return []scriptedTurn{
		{calls: []model.ToolCall{call("write", map[string]string{"path": filepath.Join(dir, "new.txt"), "content": "x"})}},
		{text: "ok"},
	}
}

// outcome returns the approved or denied event for callID, and fails when
// there is none or it comes after session.ended.
func outcome(t *testing.T, evs []Event, callID string) (Event, map[string]string) {
	t.Helper()
	ended := false
	for _, e := range evs {
		if e.Type == EvSessionEnded {
			ended = true
		}
		if e.Type != EvActionApproved && e.Type != EvActionDenied {
			continue
		}
		var p map[string]string
		_ = json.Unmarshal(e.Payload, &p)
		if p["call_id"] == callID {
			if ended {
				t.Fatalf("%s for %s recorded after session.ended", e.Type, callID)
			}
			return e, p
		}
	}
	t.Fatalf("no outcome recorded for %s: %s", callID, types(evs))
	return Event{}, nil
}

func types(evs []Event) string {
	var out []string
	for _, e := range evs {
		if e.Type != EvAgentDelta {
			out = append(out, string(e.Type))
		}
	}
	return strings.Join(out, " → ")
}

// A request that ends while its approval is awaited still records an
// outcome, saying why no one answered.
func TestUnansweredApprovalRecordsAnOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		want   TerminalReason
		reason string
	}{
		{"interrupt", nil, TermUserInterrupt, "interrupted before an answer"},
		{"shutdown", ErrShutdown, TermShutdown, "server shut down before an answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tempDir(t)
			l, store := harnessIn(t, dir, writeTurns(dir), policy.ModeDefault, false)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			l.Approver = approverFunc(func(actx context.Context, _ policy.Result) (bool, error) {
				cancel(tc.cause)
				<-actx.Done()
				return false, actx.Err()
			})
			reason, err := l.Run(ctx, "write it")
			if err != nil || reason != tc.want {
				t.Fatalf("Run = %s, %v; want %s", reason, err, tc.want)
			}
			evs, _ := store.Events("sess1")
			e, p := outcome(t, evs, "cwrite")
			if e.Type != EvActionDenied || e.Actor != ActorSystem || p["by"] != BySystem ||
				p["step"] != "ask" || p["reason"] != tc.reason {
				t.Fatalf("outcome %s %s %v", e.Type, e.Actor, p)
			}
		})
	}
}

// The record names who settled a call: a reviewer asked, a scope remembered
// from an earlier "always allow", or a run with no one to ask.
func TestApprovalAttribution(t *testing.T) {
	for _, tc := range []struct {
		name      string
		approver  Approver
		typ       EventType
		actor     Actor
		by, scope string
		reason    string
	}{
		{"reviewer allows", approverFunc(func(context.Context, policy.Result) (bool, error) { return true, nil }),
			EvActionApproved, ActorSystem, ByReviewer, "", ""},
		{"reviewer refuses", approverFunc(func(context.Context, policy.Result) (bool, error) { return false, nil }),
			EvActionDenied, ActorUser, ByReviewer, "", "rejected: "},
		{"remembered scope", approverFunc(func(ctx context.Context, _ policy.Result) (bool, error) {
			NoteAnswer(ctx, Answer{By: BySessionScope, Scope: "write(*)"})
			return true, nil
		}), EvActionApproved, ActorSystem, BySessionScope, "write(*)", ""},
		{"headless refusal", AutoApprove{Yes: false}, EvActionDenied, ActorSystem, ByHeadless, "", "no approver: "},
		{"headless allow", AutoApprove{Yes: true}, EvActionApproved, ActorSystem, ByHeadless, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tempDir(t)
			l, store := harnessIn(t, dir, writeTurns(dir), policy.ModeDefault, false)
			l.Approver = tc.approver
			if _, err := l.Run(context.Background(), "write it"); err != nil {
				t.Fatal(err)
			}
			evs, _ := store.Events("sess1")
			e, p := outcome(t, evs, "cwrite")
			if e.Type != tc.typ || e.Actor != tc.actor || p["by"] != tc.by || p["scope"] != tc.scope ||
				!strings.HasPrefix(p["reason"], tc.reason) {
				t.Fatalf("outcome %s %s %v", e.Type, e.Actor, p)
			}
		})
	}
}

// failingAdapter is stopped before it can reply, and fails the way a real
// adapter does when its request is cancelled.
type failingAdapter struct {
	scriptedAdapter
	stop func()
}

func (f *failingAdapter) Complete(ctx context.Context, _ model.Request) (<-chan model.Chunk, error) {
	f.stop()
	return nil, ctx.Err()
}

// A turn stopped before the model's first reply ends as the stop it was, not
// as an error.
func TestStopBeforeTheFirstReplyIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  TerminalReason
	}{
		{"interrupt", nil, TermUserInterrupt},
		{"shutdown", ErrShutdown, TermShutdown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := tools.NewSession(tempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			store := NewMemStore()
			l := NewLoop(&failingAdapter{stop: func() { cancel(tc.cause) }}, tools.NewRegistry(tools.Read{}),
				policy.New(policy.ModeAuto), AutoApprove{}, sess, NewRecorder(store, "sess1", ""), DefaultConfig())
			reason, err := l.Run(ctx, "go")
			if err != nil || reason != tc.want {
				t.Fatalf("Run = %s, %v; want %s", reason, err, tc.want)
			}
			evs, _ := store.Events("sess1")
			for _, e := range evs {
				var mc ModelCall
				if e.Type == EvModelCall && json.Unmarshal(e.Payload, &mc) == nil && mc.Error != "" {
					t.Fatalf("model.call recorded %q for a stop", mc.Error)
				}
				var end SessionEnded
				if e.Type == EvSessionEnded && (json.Unmarshal(e.Payload, &end) != nil || end.Reason != tc.want) {
					t.Fatalf("session.ended %s", e.Payload)
				}
			}
		})
	}
}

// A message queued for a turn that a shutdown ends is recorded as dropped,
// and not as a user.message the model never read.
func TestShutdownRecordsQueuedMessagesAsDropped(t *testing.T) {
	dir := tempDir(t)
	l, store := harnessIn(t, dir, writeTurns(dir), policy.ModeDefault, false)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var qid string
	l.Approver = approverFunc(func(actx context.Context, _ policy.Result) (bool, error) {
		qid = l.QueueMessage(Message{Text: "also do this", ClientID: "m1"})
		cancel(ErrShutdown)
		<-actx.Done()
		return false, actx.Err()
	})
	if reason, _ := l.Run(ctx, "write it"); reason != TermShutdown {
		t.Fatalf("reason %s", reason)
	}
	evs, _ := store.Events("sess1")
	var dropped *DroppedMessage
	for _, e := range evs {
		switch e.Type {
		case EvMessageDropped:
			var d DroppedMessage
			_ = json.Unmarshal(e.Payload, &d)
			dropped = &d
		case EvUserMessage:
			if strings.Contains(string(e.Payload), "also do this") {
				t.Fatal("a message the model never read was recorded as delivered")
			}
		case EvSessionEnded:
			if dropped == nil {
				t.Fatalf("no message.dropped before the end: %s", types(evs))
			}
		}
	}
	if dropped == nil || dropped.QueueID != qid || dropped.ClientID != "m1" || dropped.Text != "also do this" ||
		dropped.Reason == "" || dropped.QueuedAt.IsZero() {
		t.Fatalf("dropped %+v, queue id %s", dropped, qid)
	}
	if len(l.Queued()) != 0 {
		t.Fatal("the dropped message is still queued")
	}
}

// An interrupt leaves a queued message waiting for the next run, as before.
func TestInterruptKeepsQueuedMessages(t *testing.T) {
	dir := tempDir(t)
	l, store := harnessIn(t, dir, writeTurns(dir), policy.ModeDefault, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Approver = approverFunc(func(actx context.Context, _ policy.Result) (bool, error) {
		l.QueueMessage(Message{Text: "also do this"})
		cancel()
		<-actx.Done()
		return false, actx.Err()
	})
	_, _ = l.Run(ctx, "write it")
	evs, _ := store.Events("sess1")
	if hasEvent(evs, EvMessageDropped) || len(l.Queued()) != 1 {
		t.Fatalf("an interrupt dropped the queue: %s", types(evs))
	}
}

// A call to a tool that does not exist is on the record as a refused call.
func TestUnknownToolIsRecorded(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("nonexistent", map[string]string{})}},
		{text: "oh"},
	}, policy.ModeDefault, true)
	if _, err := l.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	var requested bool
	for _, e := range evs {
		var a ActionRequested
		if e.Type == EvActionRequested && json.Unmarshal(e.Payload, &a) == nil && a.CallID == "cnonexistent" {
			requested = a.Tool == "nonexistent" && !a.RequiresApproval
		}
	}
	e, p := outcome(t, evs, "cnonexistent")
	if !requested || e.Type != EvActionDenied || e.Actor != ActorSystem || p["step"] != "unknown" || p["by"] != BySystem {
		t.Fatalf("requested %v, outcome %s %v", requested, e.Type, p)
	}
}

// An approver that fails, with the run still live, ends it as an error and
// records why the request went unanswered.
func TestFailedApproverRecordsAnOutcome(t *testing.T) {
	dir := tempDir(t)
	l, store := harnessIn(t, dir, writeTurns(dir), policy.ModeDefault, false)
	l.Approver = approverFunc(func(context.Context, policy.Result) (bool, error) {
		return false, errors.New("approval store unreachable")
	})
	if reason, _ := l.Run(context.Background(), "write it"); reason != TermError {
		t.Fatalf("reason %s", reason)
	}
	evs, _ := store.Events("sess1")
	e, p := outcome(t, evs, "cwrite")
	if e.Type != EvActionDenied || p["by"] != BySystem || !strings.Contains(p["reason"], "approval store unreachable") {
		t.Fatalf("outcome %s %v", e.Type, p)
	}
}

// A call refused before approval, because it could not succeed, still has an
// outcome on the record.
func TestPrecheckRefusalRecordsAnOutcome(t *testing.T) {
	l, store, _ := harness(t, []scriptedTurn{
		{calls: []model.ToolCall{call("write", map[string]string{"path": "new.txt", "content": "x"})}},
		{text: "ok"},
	}, policy.ModeDefault, true)
	if _, err := l.Run(context.Background(), "write a file"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	e, p := outcome(t, evs, "cwrite")
	if e.Type != EvActionDenied || e.Actor != ActorSystem || p["by"] != BySystem || p["step"] != "precheck" ||
		!strings.Contains(p["reason"], "absolute") {
		t.Fatalf("outcome %s %s %v", e.Type, e.Actor, p)
	}
}

// A person's call that policy denies says so in by, as the agent's do.
func TestManualDenialSaysByPolicy(t *testing.T) {
	l, store := manualLoop(t)
	if _, refused, err := l.ManualAuthorize("bash", "u1", bashArgs("curl https://example.com")); err != nil || refused == nil {
		t.Fatalf("refused %v err %v", refused, err)
	}
	ds := decisionsOf(t, store, "u1")
	var p map[string]string
	if len(ds) == 1 {
		_ = json.Unmarshal(ds[0].Payload, &p)
	}
	if len(ds) != 1 || ds[0].Type != EvActionDenied || p["by"] != ByPolicy {
		t.Fatalf("decisions %v", p)
	}
}

// A run's own time limit is neither an interrupt nor a model failure.
func TestDeadlineBeforeTheFirstReply(t *testing.T) {
	sess, err := tools.NewSession(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	store := NewMemStore()
	l := NewLoop(&failingAdapter{stop: func() {}}, tools.NewRegistry(tools.Read{}),
		policy.New(policy.ModeAuto), AutoApprove{}, sess, NewRecorder(store, "sess1", ""), DefaultConfig())
	// The turn itself, past the loop's own check, so the model call sees the deadline.
	reason, done, err := l.turn(ctx)
	if err != nil || !done || reason != TermDeadline {
		t.Fatalf("turn = %s, %v, %v; want deadline", reason, done, err)
	}
	if reason, _ := l.Run(ctx, "go"); reason != TermDeadline {
		t.Fatalf("Run = %s; want deadline", reason)
	}
	if TermDeadline.ExitCode() == TermUserInterrupt.ExitCode() || TermDeadline.ExitCode() == TermError.ExitCode() {
		t.Fatal("deadline shares an exit code")
	}
}

// An answer that arrived as the wait was stopped is reported as not applied.
func TestHeldAnswerLostToAnInterrupt(t *testing.T) {
	dir := tempDir(t)
	l, store := harnessIn(t, dir, writeTurns(dir), policy.ModeDefault, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Approver = approverFunc(func(actx context.Context, _ policy.Result) (bool, error) {
		cancel()
		NoteAnswer(actx, Answer{Held: true})
		return false, actx.Err()
	})
	_, _ = l.Run(ctx, "write it")
	evs, _ := store.Events("sess1")
	if _, p := outcome(t, evs, "cwrite"); p["reason"] != "interrupted before the answer was applied" {
		t.Fatalf("reason %q", p["reason"])
	}
}

// A By that is none of the By values is not recorded; the answer is a reviewer's.
func TestUnknownByIsIgnored(t *testing.T) {
	dir := tempDir(t)
	l, store := harnessIn(t, dir, writeTurns(dir), policy.ModeDefault, false)
	l.Approver = approverFunc(func(ctx context.Context, _ policy.Result) (bool, error) {
		NoteAnswer(ctx, Answer{By: "the-moon", Scope: "write(*)"})
		return true, nil
	})
	if _, err := l.Run(context.Background(), "write it"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	if _, p := outcome(t, evs, "cwrite"); p["by"] != ByReviewer || p["scope"] != "" {
		t.Fatalf("outcome %v", p)
	}
}
