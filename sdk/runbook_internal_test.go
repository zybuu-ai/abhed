package abhed

import "testing"

// Options.Runbook's steps go with the first run, whichever entry point
// starts it, and only once.
func TestRunbookNoteGoesOnce(t *testing.T) {
	a := &Agent{runbookNote: "the steps"}
	if got := a.withRunbook("go"); got != "go\n\nthe steps" {
		t.Fatalf("first: %q", got)
	}
	if got := a.withRunbook("again"); got != "again" {
		t.Fatalf("second: %q", got)
	}
}
