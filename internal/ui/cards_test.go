package ui

import (
	"strings"
	"testing"
	"time"
)

// The plan card shows the plan as markdown and its four answers; nothing is
// chosen by Enter alone, and a deliberate choice returns its ID.
func TestPlanCard(t *testing.T) {
	spec := PlanCard("## Plan\n\n1. Read the file.\n2. Change the **greeting**.")
	if _, err := spec.Normalized(); err != nil {
		t.Fatal(err)
	}
	dr := openDialog(t, spec)
	for _, want := range []string{"Plan ready", "Plan", "1. Read the file.", "Change the greeting.",
		"1. Yes, and accept edits without asking", "2. Yes, and ask before each change", "3. No, keep planning", "4. No, and tell Abhed what to change (esc)"} {
		if !strings.Contains(dr.term.Text(), want) {
			t.Errorf("the card lacks %q:\n%s", want, dr.term.Dump())
		}
	}
	dr.clock.advance(time.Second)
	dr.key("\r")
	if id, ok := dr.answered(); ok {
		t.Fatalf("Enter alone answered %q", id)
	}
	dr.clock.advance(time.Second)
	dr.key("2")
	dr.timers.advance(approvalGuard)
	if id, _ := dr.answered(); id != PlanApprove {
		t.Fatalf("2 answered %q", id)
	}
	dr.waitText("Plan approved · each change is asked about")
}

// Esc on a plan card means "tell Abhed what to change".
func TestPlanCardEscRefines(t *testing.T) {
	dr := openDialog(t, PlanCard("a plan"))
	dr.clock.advance(time.Second)
	dr.key("\x1b")
	if id, _ := dr.answered(); id != PlanRefine {
		t.Fatalf("Esc answered %q", id)
	}
}

// Switching to auto asks first, and Enter says No.
func TestModeConfirm(t *testing.T) {
	dr := openDialog(t, ModeConfirm("default", "auto"))
	if !strings.Contains(dr.term.Text(), "destructive ones always do") {
		t.Fatalf("the confirm does not say what auto does:\n%s", dr.term.Dump())
	}
	dr.clock.advance(time.Second)
	dr.key("\r")
	if id, _ := dr.answered(); id != ChoiceNo {
		t.Fatalf("Enter answered %q", id)
	}
}
