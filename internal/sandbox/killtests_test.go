package sandbox

import (
	"os"
	"runtime"
	"testing"
)

// requireKillTests skips unless ABHED_TEST_PROCESS_KILL=1: the test ends real
// process trees, which is done only on a throwaway runner or in a container.
func requireKillTests(t *testing.T) {
	t.Helper()
	if os.Getenv("ABHED_TEST_PROCESS_KILL") != "1" {
		t.Skip("set ABHED_TEST_PROCESS_KILL=1 on a throwaway runner or in a container to run it")
	}
}

// requireKillTestsOnMac is requireKillTests on macOS, where these tests would
// otherwise run on a person's own machine; Linux runs them in CI's containers.
func requireKillTestsOnMac(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		requireKillTests(t)
	}
}
