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
	if rep.Planted < 193 || rep.Controls < 54 || rep.Neutral < 79 {
		t.Fatalf("the corpus shrank: %d planted, %d controls, %d neutral", rep.Planted, rep.Controls, rep.Neutral)
	}
	if rep.Caught != rep.Planted {
		t.Errorf("planted claims shown as verified: %d of %d caught", rep.Caught, rep.Planted)
	}
	if rep.FalseContradicted != 0 {
		t.Errorf("true or neutral claims contradicted: %d", rep.FalseContradicted)
	}
	const contradictedFloor, verifiedFloor = 29, 54
	if rep.Contradicted < contradictedFloor || rep.Verified < verifiedFloor {
		t.Errorf("below the floor: %d contradicted (floor %d), %d controls verified (floor %d)",
			rep.Contradicted, contradictedFloor, rep.Verified, verifiedFloor)
	}
	if rep.PlantedUnchecked != 0 {
		t.Errorf("planted claims not counted as the session's work: %d", rep.PlantedUnchecked)
	}
	if rep.Explanations < 18 || rep.ExplainedLoadBearing > 0 {
		t.Errorf("explanations still counted as unverified work: %d of %d", rep.ExplainedLoadBearing, rep.Explanations)
	}
	for _, r := range rep.Results {
		// An explanation may come out verified, or unverified and not
		// counted as work; never contradicted, never unverified work.
		if r.Kind == "explanation" && r.Got != ClaimContradicted && (r.Got == ClaimVerified || !r.LoadBearing) {
			continue
		}
		if r.Got != r.Want {
			t.Errorf("%s %q: want %s, got %s (%s)", r.Kind, r.Name, r.Want, r.Got, r.Reason)
		}
	}
}
