package model

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastRetry() RetryPolicy {
	return RetryPolicy{Attempts: 4, Base: 5 * time.Millisecond, Max: 50 * time.Millisecond}
}

// A rate limit is a condition of the moment, not of the request. Ending the
// session on one discards everything it had established, which is what a user
// reads as "the agent crashed" when nothing was wrong with what they asked.
func TestRateLimitIsRetriedNotFatal(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "k", "m", Profile{})
	a.Retry = fastRetry()
	ch, err := a.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatalf("a 429 that clears must not fail the turn: %v", err)
	}
	for range ch {
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("made %d attempts, want 3 (two rate limited, one served)", got)
	}
}

// The body has to be rebuilt on each attempt. Reusing the request retries with
// a drained reader, and the server rejects it for a reason unrelated to the
// original failure.
func TestRetrySendsTheBodyEveryTime(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "k", "m", Profile{})
	a.Retry = fastRetry()
	ch, err := a.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(bodies))
	}
	if bodies[0] == "" || bodies[0] != bodies[1] {
		t.Errorf("the retry sent a different or empty body:\n  first: %.60s\n  retry: %.60s",
			bodies[0], bodies[1])
	}
}

// A bad credential fails identically however often it is sent. Retrying only
// delays the error the user needs to read.
func TestClientErrorsAreNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "bad", "m", Profile{})
	a.Retry = fastRetry()
	if _, err := a.Complete(context.Background(), Request{}); err == nil {
		t.Fatal("a 401 must be reported, not swallowed")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("made %d attempts on a 401; it must not be retried", got)
	}
}

// The server knows when its limit resets; a client guessing shorter just burns
// another request against the same limit.
func TestRetryAfterIsHonoured(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "k", "m", Profile{})
	// Base is far shorter than the advised second, so honouring the header is
	// observable in the elapsed time.
	a.Retry = RetryPolicy{Attempts: 3, Base: time.Millisecond, Max: 5 * time.Second}

	start := time.Now()
	ch, err := a.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("waited %s; Retry-After: 1 was ignored in favour of the shorter backoff", elapsed)
	}
}

// Giving up must say how hard it tried, or the message implies a single try.
func TestExhaustedRetriesReportTheAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "k", "m", Profile{})
	a.Retry = fastRetry()
	_, err := a.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("a persistent rate limit must eventually be reported")
	}
	if !strings.Contains(err.Error(), "after 4 attempts") {
		t.Errorf("error should name the attempts, got: %v", err)
	}
}

// A user watching a prompt needs to know why it has gone quiet.
func TestRetryNotifiesTheUser(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	var notices []string
	a := NewAnthropic(srv.URL, "k", "m", Profile{})
	a.Retry = fastRetry()
	a.Notify = func(m string) { notices = append(notices, m) }

	ch, _ := a.Complete(context.Background(), Request{})
	for range ch {
	}
	if len(notices) == 0 {
		t.Fatal("a silent pause is indistinguishable from a hang")
	}
	if !strings.Contains(notices[0], "rate limited") {
		t.Errorf("notice = %q, want it to say what happened", notices[0])
	}
}

// Cancelling during a backoff must return promptly, not sleep it out.
func TestRetryRespectsCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "k", "m", Profile{})
	a.Retry = RetryPolicy{Attempts: 3, Base: time.Second, Max: 30 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := a.Complete(ctx, Request{}); err == nil {
		t.Fatal("want an error when the context expires mid-backoff")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %s to notice cancellation; a user interrupt must be prompt", elapsed)
	}
}

func TestRetryableStatuses(t *testing.T) {
	for _, s := range []int{429, 500, 502, 503, 504} {
		if !Retryable(s) {
			t.Errorf("%d %s should be retried", s, http.StatusText(s))
		}
	}
	for _, s := range []int{200, 400, 401, 403, 404, 422} {
		if Retryable(s) {
			t.Errorf("%d %s must not be retried", s, http.StatusText(s))
		}
	}
	_ = fmt.Sprint()
}

// A subscription token is accepted and then refused for anything but
// Anthropic's own apps, and the refusal arrives as a 429. Telling the user to
// wait for a limit that will never clear sends them to look in the wrong place.
func TestSubscriptionRefusalIsExplainedNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// No Retry-After, no ratelimit headers: what the refusal looks like.
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "", "claude-opus-5", Profile{})
	a.Bearer = "sk-ant-oat01-example"
	a.Retry = fastRetry()

	_, err := a.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("want an error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("made %d attempts; a limit that names no reset will not clear", got)
	}
	msg := err.Error()
	for _, want := range []string{"restricted to Anthropic's own apps", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error should say %q and what to do instead:\n%s", want, msg)
		}
	}
}

// A real rate limit must still be retried and reported as one. Misreporting it
// as a policy refusal would send the user to change a credential that is fine.
func TestRealRateLimitIsStillRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("anthropic-ratelimit-requests-remaining", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "", "m", Profile{})
	a.Bearer = "sk-ant-oat01-example"
	a.Retry = fastRetry()

	ch, err := a.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatalf("a real rate limit that clears must not be reported as a refusal: %v", err)
	}
	for range ch {
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// An API key hitting the same 429 is a rate limit, not a subscription problem.
func TestAPIKeyIsNotToldAboutSubscriptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "sk-ant-api03-key", "m", Profile{})
	a.Retry = fastRetry()
	_, err := a.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "restricted to Anthropic's own apps") {
		t.Errorf("an API key must not be told its subscription is restricted:\n%s", err)
	}
}
