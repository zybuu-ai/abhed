package model

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// hanging serves a stub model that sends head, then holds the reply open
// until the test ends; it counts the requests it got.
func hanging(t *testing.T, head func(w http.ResponseWriter)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if head != nil {
			head(w)
		}
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	return srv, &n
}

func stalled(t *testing.T, err error, stall bool) {
	t.Helper()
	var te *TimeoutError
	if !errors.As(err, &te) || te.Stall != stall {
		t.Fatalf("got %v, want a TimeoutError with Stall %v", err, stall)
	}
}

// A model that never answers is given up on after the stall timeout, once,
// not retried four times as a failed connection is.
func TestStallBeforeAnyByteEndsTheCallOnce(t *testing.T) {
	srv, n := hanging(t, nil)
	c := NewOpenAICompatible(srv.URL, "k", "m", Profile{})
	c.SetTimeouts(Timeouts{Stall: 200 * time.Millisecond})
	start := time.Now()
	_, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	stalled(t, err, true)
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("took %s", el)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("%d requests, want 1", got)
	}
}

// A reply that starts and then goes quiet ends the stream with the stall error.
func TestStallMidStreamEndsTheStream(t *testing.T) {
	srv, _ := hanging(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		w.(http.Flusher).Flush()
	})
	c := NewOpenAICompatible(srv.URL, "k", "m", Profile{})
	c.SetTimeouts(Timeouts{Stall: 200 * time.Millisecond})
	stream, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for ch := range stream {
		if ch.Type == ChunkError {
			got = ch.Err
		}
	}
	stalled(t, got, true)
}

// Bytes that keep coming hold off the stall timeout but not the call timeout.
func TestCallTimeoutBoundsATrickle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for {
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()
	c := NewOpenAICompatible(srv.URL, "k", "m", Profile{})
	c.SetTimeouts(Timeouts{Call: 600 * time.Millisecond, Stall: 300 * time.Millisecond})
	stream, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for ch := range stream {
		if ch.Type == ChunkError {
			got = ch.Err
		}
	}
	stalled(t, got, false)
}

// The caller's own cancel is not reported as a timeout.
func TestCallerCancelIsNotATimeout(t *testing.T) {
	srv, _ := hanging(t, nil)
	c := NewOpenAICompatible(srv.URL, "k", "m", Profile{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Complete(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	var te *TimeoutError
	if err == nil || errors.As(err, &te) {
		t.Fatalf("got %v, want the caller's cancel", err)
	}
}

// The configured bounds reach the adapter, and a negative one is refused.
func TestSpecTimeouts(t *testing.T) {
	if _, err := New(Spec{Type: "openai-compatible", BaseURL: "http://x", Model: "m", Timeouts: Timeouts{Stall: -1}}); err == nil {
		t.Fatal("a negative timeout was accepted")
	}
	a, err := New(Spec{Type: "openai-compatible", BaseURL: "http://x", Model: "m", Timeouts: Timeouts{Stall: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := a.(*OpenAICompatible).HTTP.Transport.(*timeoutTransport)
	if !ok || tr.t.Stall != time.Second || tr.t.Call != DefaultTimeouts().Call {
		t.Fatalf("transport %+v", a.(*OpenAICompatible).HTTP.Transport)
	}
}
