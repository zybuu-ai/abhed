package config

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/testenv"
)

// TestMain keeps the tests off this machine's own configuration, managed
// files and model services (internal/testenv); a test that needs one sets its own.
func TestMain(m *testing.M) { testenv.Main(m) }

// freshWarnings gives the test its own once-per-process warning set, so a
// warning a run already printed is printed again; the set is restored after.
func freshWarnings(t testing.TB) {
	t.Helper()
	warnedMu.Lock()
	saved := warned
	warned = map[string]bool{}
	warnedMu.Unlock()
	t.Cleanup(func() {
		warnedMu.Lock()
		warned = saved
		warnedMu.Unlock()
	})
}
