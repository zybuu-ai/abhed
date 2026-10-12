package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A subscription token and an API key are different credentials on different
// headers. Sending both would let the server choose, which makes "which account
// paid for this" depend on someone else's precedence rules rather than on what
// the operator configured.
func TestAnthropicSendsBearerForASubscriptionToken(t *testing.T) {
	var gotAuth, gotKey, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("x-api-key")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "sk-should-not-be-sent", "m", Profile{})
	a.Bearer = "oauth-token"
	ch, err := a.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	if gotAuth != "Bearer oauth-token" {
		t.Errorf("Authorization = %q, want the bearer token", gotAuth)
	}
	if gotKey != "" {
		t.Errorf("x-api-key = %q; both credentials must never be sent together", gotKey)
	}
	if !strings.Contains(gotBeta, "oauth-2025-04-20") {
		t.Errorf("anthropic-beta = %q, want the oauth beta that makes a "+
			"subscription token acceptable", gotBeta)
	}
}

func TestAnthropicSendsAPIKeyWhenThereIsNoToken(t *testing.T) {
	var gotAuth, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotKey = r.Header.Get("Authorization"), r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "sk-key", "m", Profile{})
	ch, _ := a.Complete(context.Background(), Request{})
	for range ch {
	}
	if gotKey != "sk-key" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want it unset for an API key", gotAuth)
	}
}

// An operator's own beta flags must survive alongside the one a subscription
// token requires.
func TestBetaHeaderKeepsConfiguredFlags(t *testing.T) {
	got := betaHeader([]string{"context-management-2025-06-27"}, "oauth-2025-04-20")
	if !strings.Contains(got, "oauth-2025-04-20") ||
		!strings.Contains(got, "context-management-2025-06-27") {
		t.Fatalf("betaHeader() = %q, want both flags", got)
	}
	// And it must not be added twice when already named.
	twice := betaHeader([]string{"oauth-2025-04-20"}, "oauth-2025-04-20")
	if strings.Count(twice, "oauth-2025-04-20") != 1 {
		t.Errorf("betaHeader() = %q, want the flag once", twice)
	}
}

// Abhed's own variable wins over the setup-token name, and config wins over both.
func TestAnthropicSubscriptionTokenPrecedence(t *testing.T) {
	cases := []struct {
		name, cfg, own, legacy, auth, want string
	}{
		{name: "own beats legacy", own: "own", legacy: "legacy", auth: "auth", want: "own"},
		{name: "legacy fallback", legacy: "legacy", auth: "auth", want: "legacy"},
		{name: "auth token last", auth: "auth", want: "auth"},
		{name: "config first", cfg: "cfg", own: "own", legacy: "legacy", want: "cfg"},
		{name: "none", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ABHED_ANTHROPIC_OAUTH_TOKEN", c.own)
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", c.legacy)
			t.Setenv("ANTHROPIC_AUTH_TOKEN", c.auth)
			s := Spec{Type: "anthropic", Model: "m"}
			if c.cfg != "" {
				s.Extra = map[string]string{"oauth_token": c.cfg}
			}
			ad, err := newAnthropicAdapter(s)
			if err != nil {
				t.Fatal(err)
			}
			if got := ad.(*Anthropic).Bearer; got != c.want {
				t.Errorf("Bearer = %q, want %q", got, c.want)
			}
		})
	}
}
