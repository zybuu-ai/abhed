package policy

import (
	"encoding/json"
	"testing"
)

// A read-only tool that can carry data out asks in the modes that would run
// it unasked, plan included, unless an allow rule names it; bypass runs it.
func TestAskReadOnlyAsksUnlessAllowed(t *testing.T) {
	const why = "web_fetch asks: no allowed_hosts configured"
	args, _ := json.Marshal(map[string]string{"url": "https://example.com/weather"})
	for _, tc := range []struct {
		mode  Mode
		allow []string
		want  Decision
		step  string
	}{
		{ModeDefault, nil, Ask, "default"},
		{ModeAcceptEdits, nil, Ask, "default"},
		{ModeAuto, nil, Ask, "default"},
		{ModeAuto, []string{"web_fetch(https://example.com/*)"}, Allow, "allow"},
		{ModeDefault, []string{"web_fetch(https://example.com/*)"}, Allow, "allow"},
		{ModeDefault, []string{"web_fetch(https://other.com/*)"}, Ask, "default"},
		{ModePlan, nil, Ask, "default"},
		{ModePlan, []string{"web_fetch(https://example.com/*)"}, Allow, "allow"},
		{ModeBypass, nil, Allow, "mode"},
	} {
		e := New(tc.mode)
		e.AskReadOnly = map[string]func(string) string{"web_fetch": func(string) string { return why }}
		if err := e.AddAllow(tc.allow...); err != nil {
			t.Fatal(err)
		}
		got := e.Evaluate("web_fetch", false, args)
		if got.Decision != tc.want || got.Step != tc.step {
			t.Errorf("%s %v: %s at %s (%s), want %s at %s", tc.mode, tc.allow, got.Decision, got.Step, got.Reason, tc.want, tc.step)
		}
		if got.Decision == Ask {
			if got.Reason != why {
				t.Errorf("%s: reason %q", tc.mode, got.Reason)
			}
			if got.Offer() != "web_fetch(https://example.com/*)" {
				t.Errorf("%s: offers %q, want the site", tc.mode, got.Offer())
			}
		}
		// Another read-only tool is untouched.
		if d := e.Evaluate("read", false, json.RawMessage(`{"path":"a"}`)).Decision; d != Allow {
			t.Errorf("%s: read %s", tc.mode, d)
		}
	}

	// Without the setting, web_fetch runs as any read-only tool does.
	if d := New(ModeDefault).Evaluate("web_fetch", false, args).Decision; d != Allow {
		t.Errorf("unset: %s", d)
	}
	// A deny rule still wins.
	e := New(ModeBypass)
	e.AskReadOnly = map[string]func(string) string{"web_fetch": func(string) string { return why }}
	_ = e.AddDeny("web_fetch(https://example.com/*)")
	if d := e.Evaluate("web_fetch", false, args).Decision; d != Deny {
		t.Errorf("deny: %s", d)
	}
}
