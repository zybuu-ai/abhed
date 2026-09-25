package server

import (
	"regexp"
	"strings"
	"testing"
)

// The landing page is a single self-contained document with no build step, so
// nothing type-checks it. These tests assert the properties that actually
// broke in practice.

// The reported bug: the ten-second refresh rebuilt the call-to-action on every
// poll, so a half-filled sign-up form was replaced with a fresh sign-in form
// while the user was still typing. The guard is a state comparison; without it
// cta.textContent is cleared unconditionally.
func TestLandingDoesNotRebuildFormOnPoll(t *testing.T) {
	clear := strings.Index(landingHTML, "cta.textContent = '';")
	if clear < 0 {
		t.Fatal("cannot find the CTA reset; this test needs updating")
	}
	// The guard must appear before the reset, and return early when unchanged.
	guard := strings.Index(landingHTML, "cta.dataset.state === state")
	if guard < 0 {
		t.Fatal("the CTA is rebuilt with no state guard: a poll will destroy " +
			"a sign-in or sign-up form the user is filling in")
	}
	if guard > clear {
		t.Error("the state guard runs after the CTA is cleared, so it cannot " +
			"prevent the rebuild")
	}
}

// renderFacts must exist and be called from both branches, or the live numbers
// stop updating once the guard short-circuits load().
func TestLandingStillRefreshesFacts(t *testing.T) {
	if !strings.Contains(landingHTML, "function renderFacts(") {
		t.Fatal("renderFacts is missing")
	}
	if n := strings.Count(landingHTML, "renderFacts(o)"); n < 2 {
		t.Errorf("renderFacts called %d time(s); both the guarded early return "+
			"and the full render need it, or the activity counters freeze", n)
	}
}

// Every function the page calls at load must be defined in the page. A missing
// one is a silent no-op in the browser: the console renders, minus a section.
func TestLandingDefinesEveryFunctionItCalls(t *testing.T) {
	for _, fn := range []string{
		"load", "renderFacts", "signInForm", "signUpForm",
		"swapCard", "oauthButtons", "authValue", "authDetail", "card", "stat",
	} {
		if !strings.Contains(landingHTML, "function "+fn+"(") {
			t.Errorf("%s() is called but never defined", fn)
		}
	}
}

// The sign-up path must not appear unless the server said self-registration is
// enabled — the button is the only thing standing between a reachable port and
// an open registration form.
func TestLandingGatesSignupOnServerFlag(t *testing.T) {
	if !strings.Contains(landingHTML, "o.allow_signup") {
		t.Error("the sign-up link is not gated on allow_signup")
	}
}

// A theme token defined only inside a media query renders one theme's text on
// the other theme's background. Every custom property must have a value on the
// bare :root before any override.
func TestLandingDefinesThemeTokensOnRoot(t *testing.T) {
	start := strings.Index(landingHTML, ":root{")
	if start < 0 {
		t.Fatal("landing page has no :root block")
	}
	root := landingHTML[start:]
	root = root[:strings.Index(root, "}")+1]

	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`var\((--[a-z0-9-]+)`).
		FindAllStringSubmatch(landingHTML, -1) {
		used[m[1]] = true
	}
	for token := range used {
		if !strings.Contains(root, token+":") {
			t.Errorf("%s is used but never defined on the bare :root, so it is "+
				"undefined in the default (system-theme) state", token)
		}
	}
}
