package redteam

import (
	"os"
	"testing"

	"github.com/zybuu-ai/abhed/internal/clitest"
)

// requireEnv, set to 1, makes a harness that is not built a failure.
const requireEnv = "ABHED_REQUIRE_CLITEST"

// start runs the binary for one invariant test.
func start(t *testing.T, o clitest.Opts) clitest.Harness {
	t.Helper()
	return clitest.Start(t, o)
}

// The suite only counts as evidence once the harness runs it. With the
// requirement set, a harness that still skips fails here.
func TestHarnessIsBuiltWhenRequired(t *testing.T) {
	skipped := false
	t.Run("start", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		h := clitest.Start(t, clitest.Opts{Script: `text "ok"`})
		h.Exit(0)
	})
	if skipped && os.Getenv(requireEnv) == "1" {
		t.Fatalf("%s=1 but the clitest harness is not built: the invariant suite ran nothing", requireEnv)
	}
}
