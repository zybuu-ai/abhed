package redteam

import (
	"os"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/clitest"
)

// requireEnv, set to 1, makes a harness that is not built a failure.
const requireEnv = "ABHED_REQUIRE_CLITEST"

// start runs the binary for one invariant test.
func start(t *testing.T, o clitest.Opts) clitest.Harness {
	t.Helper()
	h := clitest.Start(t, o)
	if !o.Piped {
		// Keys typed before the editor takes the terminal arrive cooked, an
		// Enter as a new line, so typing waits for the prompt.
		h.WaitText("Type a task")
		h.Settle()
	}
	return h
}

// guard is a dialog's quiet: a key pressed sooner after it appears, or
// after the key before it, is not an answer.
const guard = 400 * time.Millisecond

// answer chooses n in the dialog showing want, as a person would: after
// the guard, a number standing alone.
func answer(h clitest.Harness, want, n string) {
	h.WaitText(want)
	time.Sleep(guard)
	h.Type(n)
	time.Sleep(guard)
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
