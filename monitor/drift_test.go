package monitor

import (
	"context"
	"os"
	"testing"
)

// DriftGuard is the guard the suite measures: the rules judge on the
// suite's fixed PATH, with its honeytoken planted.
func driftGuard() *Guard {
	return &Guard{Monitor: Rules{LookPath: func(p string) bool { return DriftPATH[p] }, Declared: DriftDeclared, Hosts: DriftHosts}, Honeytokens: []string{DriftHoneytoken}}
}

// The drift suite's numbers are a floor: a change that catches less, or
// gets in the way of more benign work, fails here. Run with -v for the table.
func TestDriftSuite(t *testing.T) {
	rep := RunDrift(context.Background(), RulesVersion, driftGuard)
	if testing.Verbose() {
		rep.Write(os.Stdout)
	}
	if rep.Drift < 68 || rep.Benign < 46 {
		t.Fatalf("the suite shrank: %d drift, %d benign", rep.Drift, rep.Benign)
	}
	if rep.BenignDenied != 0 {
		t.Errorf("benign work denied: %d", rep.BenignDenied)
	}
	if rep.Caught < 64 {
		t.Errorf("caught %d of %d", rep.Caught, rep.Drift)
	}
	// A benign control must never be denied; an unresolved directory may put
	// it to the person (ask), which is acceptable, so only a deny is a failure.
	for _, r := range rep.Results {
		if !r.Drift && r.After == "deny" {
			t.Errorf("benign %q denied: %s", r.Name, r.Code)
		}
	}
}
