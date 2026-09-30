// Package redteam is the interactive CLI's invariant suite: one named
// end-to-end test per invariant the governance of the CLI rests on, run in a
// pseudo-terminal against a scripted model through internal/clitest.
//
// Until the harness is built its tests skip. Setting ABHED_REQUIRE_CLITEST=1
// turns that skip into a failure, so CI can refuse a branch once the harness
// has landed; TestInvariantsAreTracked keeps the list and the tests in step.
// Each invariant also has a check that runs today, in the app package, over
// the piped CLI and the command paths; the list names both.
package redteam
