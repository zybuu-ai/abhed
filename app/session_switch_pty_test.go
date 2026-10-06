package app

import (
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/clitest"
)

// twoSessions runs two prompts, a /clear between them, so the CLI is in the
// second session with the first recorded in the same workspace.
func twoSessions(t *testing.T, args ...string) clitest.Harness {
	t.Helper()
	h := startAtPrompt(t, clitest.Opts{Cols: 120, Script: "text \"one\"\n\ntext \"two\"\n", Args: append([]string{"-C", t.TempDir()}, args...)})
	for i, p := range []string{"terraform cleanup plan", "release notes draft"} {
		h.Type(p)
		h.Key(clitest.Enter)
		h.WaitText([]string{"● one", "● two"}[i])
		h.WaitScreen(func(s clitest.Screen) bool { return s.Contains("? for shortcuts") }, clitest.DefaultTimeout)
		h.Settle()
		if i == 0 {
			h.Type("/clear")
			h.Key(clitest.Enter)
			h.WaitText("context cleared")
			h.WaitScreen(func(s clitest.Screen) bool { return !s.Contains("session terraform") }, clitest.DefaultTimeout)
			h.Settle()
		}
	}
	return h
}

// The footer names the session the CLI is in, by its first prompt and then
// by the name /rename gives it.
func TestPtyFooterNamesTheSession(t *testing.T) {
	h := twoSessions(t)
	h.WaitText("session release notes draft")
	h.Type("/rename Notes for 1.2.6")
	h.Key(clitest.Enter)
	h.WaitText("named Notes for 1.2.6")
	h.WaitText("session Notes for 1.2.6")
	h.Exit(0)
}

// /resume's picker marks the session already open, filters as letters are
// typed, and Enter resumes the one match at once.
func TestPtyResumePickerFiltersAndMarksCurrent(t *testing.T) {
	h := twoSessions(t)
	h.Type("/resume")
	h.Key(clitest.Enter)
	h.WaitText("Resume which session?")
	s := h.WaitText("● current")
	if !lineWith(s, "● current", "release notes draft") {
		t.Fatalf("the current session is not the one marked:\n%s", s.Text())
	}
	h.WaitText("type to filter")
	time.Sleep(400 * time.Millisecond) // the dialog takes no key in its first moment
	h.Type("terra")
	h.WaitText("filter: terra")
	h.Key(clitest.Enter)
	h.WaitText("resumed")
	h.WaitText("session terraform cleanup plan")
	h.Exit(0)
}

// /switch is /resume by another name, and the session already open is not
// resumed again when it is picked.
func TestPtySwitchIsResume(t *testing.T) {
	h := twoSessions(t)
	h.Type("/switch")
	h.Key(clitest.Enter)
	h.WaitText("Resume which session?")
	time.Sleep(400 * time.Millisecond)
	h.Type("release")
	h.WaitText("filter: release")
	h.Key(clitest.Enter)
	h.WaitText("is the session you are in")
	if h.Screen().Contains("replaying") {
		t.Fatal("picking the open session replayed it")
	}
	h.Exit(0)
}

// lineWith reports whether one screen row holds every text.
func lineWith(s clitest.Screen, texts ...string) bool {
	for _, line := range strings.Split(s.Text(), "\n") {
		all := true
		for _, x := range texts {
			if !strings.Contains(line, x) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}
