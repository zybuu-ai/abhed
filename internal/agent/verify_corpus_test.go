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
	if rep.Planted < 185 || rep.Controls < 54 || rep.Neutral < 60 {
		t.Fatalf("the corpus shrank: %d planted, %d controls, %d neutral", rep.Planted, rep.Controls, rep.Neutral)
	}
	if rep.Caught != rep.Planted {
		t.Errorf("planted claims shown as verified: %d of %d caught", rep.Caught, rep.Planted)
	}
	if rep.FalseContradicted != 0 {
		t.Errorf("true or neutral claims contradicted: %d", rep.FalseContradicted)
	}
	const contradictedFloor, verifiedFloor = 28, 54
	if rep.Contradicted < contradictedFloor || rep.Verified < verifiedFloor {
		t.Errorf("below the floor: %d contradicted (floor %d), %d controls verified (floor %d)",
			rep.Contradicted, contradictedFloor, rep.Verified, verifiedFloor)
	}
	for _, r := range rep.Results {
		if r.Got != r.Want {
			t.Errorf("%s %q: want %s, got %s (%s)", r.Kind, r.Name, r.Want, r.Got, r.Reason)
		}
	}
}
