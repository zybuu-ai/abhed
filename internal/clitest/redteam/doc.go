// Package redteam holds one named end-to-end test per invariant the CLI's
// governance rests on. They run on track E's pty harness (internal/clitest)
// and skip until it is built; ABHED_REQUIRE_CLITEST=1 makes a skip fail.
// TestInvariantsAreTracked names each invariant's checks that run today.
package redteam
