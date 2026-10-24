package agent

import (
	"os"
	"testing"
)

// The false-claim corpus, as a ratchet: no planted claim is shown as
// verified, no true or neutral one is contradicted, and the floors below
// rise, except where a rule is made stricter on purpose.
// go test ./internal/agent -run TestFalseClaimCorpus -v prints it.
func TestFalseClaimCorpus(t *testing.T) {
	rep := RunCorpus()
	if testing.Verbose() {
		rep.Write(os.Stdout)
	}
	if rep.Planted < 364 || rep.Controls < 64 || rep.Neutral < 81 {
		t.Fatalf("the corpus shrank: %d planted, %d controls, %d neutral", rep.Planted, rep.Controls, rep.Neutral)
	}
	if rep.Caught != rep.Planted {
		t.Errorf("planted claims shown as verified: %d of %d caught", rep.Caught, rep.Planted)
	}
	if rep.FalseContradicted != 0 {
		t.Errorf("true or neutral claims contradicted: %d", rep.FalseContradicted)
	}
	const contradictedFloor, verifiedFloor = 30, 64
	if rep.Contradicted < contradictedFloor || rep.Verified < verifiedFloor {
		t.Errorf("below the floor: %d contradicted (floor %d), %d controls verified (floor %d)",
			rep.Contradicted, contradictedFloor, rep.Verified, verifiedFloor)
	}
	if rep.PlantedUnchecked != 0 {
		t.Errorf("planted claims not counted as the session's work: %d", rep.PlantedUnchecked)
	}
	// One explanation stays counted as work: an analogy that shares no word
	// with what was read ("like counting coins"); the rule fails that way.
	if rep.Explanations < 19 || rep.ExplainedLoadBearing > 1 {
		t.Errorf("explanations still counted as unverified work: %d of %d", rep.ExplainedLoadBearing, rep.Explanations)
	}
	for _, r := range rep.Results {
		// An explanation may come out verified, or unverified and not
		// counted as work; never contradicted, never unverified work.
		if r.Kind == "explanation" && r.Got != ClaimContradicted && (r.Got == ClaimVerified || !r.LoadBearing || r.Explains) {
			continue
		}
		if r.Got != r.Want {
			t.Errorf("%s %q: want %s, got %s (%s)", r.Kind, r.Name, r.Want, r.Got, r.Reason)
		}
	}
}

// The explanation group is only shown: with it off, every case comes out
// with the same verdict, the same LoadBearing, and so the same outcome.
func TestExplanationChangesNoOutcome(t *testing.T) {
	on := RunCorpus()
	explainsOn = false
	defer func() { explainsOn = true }()
	off := RunCorpus()
	grouped := 0
	for i, a := range on.Results {
		b := off.Results[i]
		if a.Got != b.Got || a.LoadBearing != b.LoadBearing {
			t.Errorf("%s %q: %s/%v with the explanation group, %s/%v without", a.Kind, a.Name, a.Got, a.LoadBearing, b.Got, b.LoadBearing)
		}
		if a.Explains {
			grouped++
		}
	}
	if grouped == 0 {
		t.Fatal("no case was grouped as explanation")
	}
}
