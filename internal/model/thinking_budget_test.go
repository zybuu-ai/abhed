package model

import "testing"

// The budget a request will carry: the request's, else the operator's, and
// none when the provider sends no budget.
func TestThinkingBudget(t *testing.T) {
	b := 32000
	a := NewAnthropic("http://unused", "k", "claude-sonnet-4-5", Profile{Sampling: Sampling{ThinkingBudget: true, Think: true}})
	a.Defaults.ThinkingBudget = &b
	if got := ThinkingBudget(a, Request{}); got != 32000 {
		t.Fatalf("configured budget %d", got)
	}
	own := 4000
	if got := ThinkingBudget(a, Request{Params: Params{ThinkingBudget: &own}}); got != 4000 {
		t.Fatalf("the request's own %d", got)
	}
	o := NewOpenAICompatible("http://unused", "", "m", Profile{})
	o.Defaults.ThinkingBudget = &b
	if got := ThinkingBudget(o, Request{}); got != 0 {
		t.Fatalf("a provider that sends no budget read %d", got)
	}
}

// Asked with thinking off, Anthropic and Gemini send no thinking, whatever
// budget is configured.
func TestThinkingOffSendsNoBudget(t *testing.T) {
	b := 32000
	off := false
	a := NewAnthropic("http://unused", "k", "claude-sonnet-4-5", Profile{Sampling: Sampling{ThinkingBudget: true, Think: true}})
	a.Defaults.ThinkingBudget = &b
	if r := a.buildRequest(Request{MaxTokens: 1000}); r.Thinking == nil {
		t.Fatal("setup: no thinking with a budget")
	}
	if r := a.buildRequest(Request{MaxTokens: 1000, Params: Params{Think: &off}}); r.Thinking != nil {
		t.Fatalf("thinking off still sent %+v", r.Thinking)
	}
	g := NewGemini("http://unused", "k", "gemini-2.5-flash", Profile{Sampling: Sampling{ThinkingBudget: true, Think: true}})
	g.Defaults.ThinkingBudget = &b
	if r := g.buildRequest(Request{Params: Params{Think: &off}}); r.GenerationConfig != nil && r.GenerationConfig.ThinkingConfig != nil {
		t.Fatalf("thinking off still sent %+v", r.GenerationConfig.ThinkingConfig)
	}
}
