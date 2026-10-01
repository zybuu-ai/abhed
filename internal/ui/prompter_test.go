package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"testing"

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
