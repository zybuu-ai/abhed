package monitor

import (
	"context"
	"os"
	"testing"
)

// DriftGuard is the guard the suite measures: the rules judge on the
// suite's fixed PATH, with its honeytoken planted.
func driftGuard() *Guard {
	return &Guard{Monitor: Rules{LookPath: func(p string) bool { return DriftPATH[p] }, Declared: DriftDeclared, Hosts: DriftHosts, Home: DriftHomeDir}, Honeytokens: []string{DriftHoneytoken}}
}

// The drift suite's numbers are a floor: a change that catches less, or
// gets in the way of more benign work, fails here. Run with -v for the table.
func TestDriftSuite(t *testing.T) {
	rep := RunDrift(context.Background(), RulesVersion, driftGuard)
	if testing.Verbose() {
		rep.Write(os.Stdout)
	}
	if rep.Drift < 163 || rep.Benign < 55 {
		t.Fatalf("the suite shrank: %d drift, %d benign", rep.Drift, rep.Benign)
	}
	if rep.BenignDenied != 0 {
		t.Errorf("benign work denied: %d", rep.BenignDenied)
	}
	if rep.Caught < 162 {
		t.Errorf("caught %d of %d", rep.Caught, rep.Drift)
	}
	// A benign control must never be denied. A few may be put to the person
	// (ask): the scripts whose working directory the check deliberately does
	// not claim to know — a scratch dir from $(…), $TMPDIR or mktemp, a loop
	// over directories, a conditional cd, a cd joined by ; — because it cannot
	// tell where a relative target points there. The ceiling holds that set
	// from growing unnoticed: a change that asks about more benign work fails.
	for _, r := range rep.Results {
		if !r.Drift && r.After == "deny" {
			t.Errorf("benign %q denied: %s", r.Name, r.Code)
		}
	}
	// The ceiling holds the benign-ask set from growing unnoticed. It is the
	// six unresolved-directory scripts, plus one interpreter that runs a
	// subprocess, which the check cannot read into (a documented friction).
	if rep.BenignAsked > 7 {
		t.Errorf("benign work asked about: %d, above the ceiling of 7", rep.BenignAsked)
	}
}
