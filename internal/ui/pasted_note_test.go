package ui

import (
	"strings"
	"testing"
)

// A paste held for a login and one held nowhere are told apart.
func TestPastedNoteTellsHeldFromUnheld(t *testing.T) {
	n := PastedNote("P1:an OpenShift token,:a GitHub token")
	if !strings.Contains(n, "an OpenShift token (P1)") || !strings.Contains(n, "for a login you approve") ||
		!strings.Contains(n, "a GitHub token; it was not sent to the model or kept in the record, and is not held anywhere") {
		t.Fatalf("%q", n)
	}
	if strings.Contains(PastedNote(":a GitHub token"), "for a login you approve") {
		t.Fatal("an unheld paste is said to be held")
	}
}
