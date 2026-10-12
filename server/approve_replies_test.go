package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// An answer naming a request that is not the one waiting is told there is
// no such pending request, not that it is no longer pending.
func TestApproveUnknownRequestSaysSo(t *testing.T) {
	h, live, _, id := approvalSession(t, false)
	c := askTurn(context.Background(), live, "ev-real")
	waitAsked(live, "ev-real")
	rec := postApprove(h, id, `{"approved":true,"request_id":"ev-made-up"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no such pending request") {
		t.Fatalf("an unknown request id = %d %s", rec.Code, rec.Body)
	}
	if code := approve(h, id, `{"approved":false,"request_id":"ev-real"}`); code != http.StatusNoContent {
		t.Fatalf("the real answer = %d", code)
	}
	<-c
}

// Bad JSON to a session the caller cannot see is 404, as any body is.
func TestApproveBadBodyToUnknownSessionIs404(t *testing.T) {
	h, _, _, _ := approvalSession(t, false)
	if rec := postApprove(h, "s-nope", `{not json`); rec.Code != http.StatusNotFound {
		t.Fatalf("bad JSON to an unknown session = %d %s, want 404", rec.Code, rec.Body)
	}
}

// An answer that arrives before its request's row is written is told to
// come back, with Retry-After.
func TestApproveWhileStillRecordingSaysWhenToRetry(t *testing.T) {
	wait := approvalReadyWait
	approvalReadyWait = 200 * time.Millisecond
	t.Cleanup(func() { approvalReadyWait = wait })
	h, live, rows, id := approvalSession(t, true)
	gate := make(chan struct{})
	// Held past the reply's wait, whatever the write's own deadline.
	rows.askHook = func(context.Context) error {
		<-gate
		return nil
	}
	defer close(gate)
	c := askTurn(context.Background(), live, "ev-slow")
	waitAsked(live, "ev-slow")
	rec := postApprove(h, id, `{"approved":true,"request_id":"ev-slow"}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("an answer before the row = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	select {
	case <-c:
	case <-time.After(5 * time.Second):
	}
}
