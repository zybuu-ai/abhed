package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A provider's refusal is read for what a person can do about it, with the
// provider's own words.
func TestProblemOf(t *testing.T) {
	for _, c := range []struct {
		status     int
		body, kind string
		says       string
	}{
		{404, `{"error":{"code":404,"message":"Gemini 2.5 Pro is no longer available to new users. Please use a newer model.","status":"NOT_FOUND"}}`, ProblemRetired, "no longer available to new users"},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"model: claude-2.0 has been retired"}}`, ProblemRetired, "claude-2.0"},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`, ProblemCredit, "credit balance"},
		{429, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota"}}`, ProblemCredit, "current quota"},
		{402, `payment required`, ProblemCredit, "payment required"},
		{401, `{"error":{"message":"Incorrect API key provided: sk-...abc."}}`, ProblemKey, "Incorrect API key"},
		{403, `{"error":{"message":"Permission denied"}}`, ProblemKey, "Permission denied"},
		{429, `{"error":{"message":"Rate limit reached for requests"}}`, ProblemRateLimit, "Rate limit"},
		{503, `upstream connect error`, ProblemUnavailable, "upstream connect error"},
		{400, `{"detail":"max_tokens is too large"}`, ProblemOther, "max_tokens"},
		{404, ``, ProblemRetired, "Not Found"},
	} {
		err := fmt.Errorf("model call failed: %w", &StatusError{Status: c.status, Body: c.body, Attempts: 1})
		p := ProblemOf(err)
		if p == nil || p.Kind != c.kind || p.Status != c.status || !strings.Contains(p.Message, c.says) {
			t.Errorf("%d %s: got %+v", c.status, c.body, p)
		}
	}
	// What a provider echoes of a key is never repeated.
	for _, body := range []string{`{"error":{"message":"API key not valid: sk-ant-api03-abcdefghijkl"}}`, `bad key AIzaSyA1234567890abcdefghijklmnopqrstuv`,
		`{"message":"invalid Bearer eyJhbGciOi.payload.sig"}`, `denied for https://x.example/v1?key=abc123def`} {
		p := ProblemOf(&StatusError{Status: 401, Body: body})
		if strings.Contains(p.Message, "abcdefghijkl") || strings.Contains(p.Message, "AIzaSy") || strings.Contains(p.Message, "eyJhbG") || strings.Contains(p.Message, "abc123def") {
			t.Errorf("a key was repeated: %q", p.Message)
		}
	}
	if ProblemOf(errors.New("dial tcp: connection refused")) != nil {
		t.Error("a dial error read as a provider's answer")
	}
	long := &StatusError{Status: 500, Body: strings.Repeat("x", 2000)}
	if p := ProblemOf(long); len([]rune(p.Message)) > 301 {
		t.Errorf("not clipped: %d", len(p.Message))
	}
}
