package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// An answer sent as soon as "answer 1-N:" shows is the approval's answer,
// even if the approver has not reached its read yet; one sent before the
// question was asked still steers.
func TestPrompterTakesAnAnswerSentAsTheQuestionShows(t *testing.T) {
	p := NewPrompter()
	if p.Deliver("3") {
		t.Fatal("a line with no question asked was taken as an answer")
	}
	p.Arm()
	if !p.Deliver("3") {
		t.Fatal("an answer sent as the question showed was not taken")
	}
	if got, ok := p.Await(context.Background()); !ok || got != "3" {
		t.Fatalf("got %q, %v", got, ok)
	}
	if p.Deliver("1") {
		t.Fatal("the approval took a line after it was answered")
	}
	p.Arm()
	p.Disarm()
	if p.Deliver("1") {
		t.Fatal("a disarmed approval took a line")
	}
}

// The approver arms before it shows each "answer 1-N:", not after.
func TestApproverArmsBeforeTheAnswerLine(t *testing.T) {
	var out bytes.Buffer
	a := &Approver{Out: &out, Session: NewAllowList()}
	p := NewPrompter()
	shown := -1
	a.Arm = func() {
		shown = bytes.Count(out.Bytes(), []byte("answer 1-"))
		p.Arm()
	}
	a.Prepare = func(ctx context.Context) (func() (string, bool), func()) {
		return func() (string, bool) { return p.Await(ctx) }, p.Disarm
	}
	done := make(chan bool, 1)
	go func() {
		ok, _ := a.Approve(context.Background(), "bash", json.RawMessage(`{"command":"ls"}`), policy.Result{})
		done <- ok
	}()
	for !p.Waiting() { // Arm makes it waiting
		runtime.Gosched()
	}
	if !p.Deliver("1") {
		t.Fatal("the armed approval did not take its answer")
	}
	if !<-done {
		t.Fatal("answer 1 did not approve")
	}
	if shown != 0 {
		t.Fatalf("armed after %d answer lines were shown, want before the first", shown)
	}
}

// Two questions asked at once take turns: the second does not share the
// first one's waiting line, so an answer reaches the question it was for.
func TestPrompterHoldsOneQuestionAtATime(t *testing.T) {
	p := NewPrompter()
	ctx := context.Background()
	release, ok := p.Hold(ctx)
	if !ok {
		t.Fatal("no hold")
	}
	second := make(chan bool, 1)
	go func() {
		r, ok := p.Hold(ctx)
		second <- ok
		r()
	}()
	select {
	case <-second:
		t.Fatal("a second question held the input beside the first")
	case <-time.After(50 * time.Millisecond):
	}
	p.Arm()
	if !p.Deliver("1") {
		t.Fatal("the first question took no answer")
	}
	if line, ok := p.Await(ctx); !ok || line != "1" {
		t.Fatalf("first got %q %v", line, ok)
	}
	release()
	if !<-second {
		t.Fatal("the second question never held the input")
	}
	// A question still waiting when input ends gives up.
	release, _ = p.Hold(ctx)
	defer release()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, ok := p.Hold(cctx); ok {
		t.Fatal("held past a cancelled context")
	}
}
