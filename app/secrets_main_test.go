package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/testenv"
)

// TestMain keeps the tests off this machine's own configuration, managed
// files, secrets and model services (internal/testenv); a test may set its
// own. A trust, conversation or session helper process is this binary run
// again by a test, and keeps the home that test prepared.
func TestMain(m *testing.M) {
	// The helpers that predate the marker keep their home by their own variables.
	if os.Getenv("ABHED_TRUST_MAIN_ARGS") != "" || os.Getenv("ABHED_CONV_KEEP_HOME") != "" || os.Getenv("ABHED_SESS_WS") != "" {
		_ = os.Setenv(testenv.Marker, "1")
	}
	g := testenv.Isolate()
	// Tests re-run this binary as their own helpers; only a test that sets it
	// sees itself inside an agent's command.
	inAgentCommand = func() string { return "" }
	// No test writes the real system log; one that checks it captures it.
	sysLogWrite = func(string) error { return nil }
	// No test reaches the host's container engine; one that needs an engine
	// puts a stand-in of its own on PATH. ABHED_REQUIRE_CONTAINER asks for the
	// real one, as CI's container job does.
	if os.Getenv("ABHED_REQUIRE_CONTAINER") == "" {
		keepOffRealEngine()
	}
	code := m.Run()
	if realEngineRan() {
		code = 1
	}
	// No test leaves a record or an export in the package's own folder.
	if left, _ := filepath.Glob("*.jsonl"); len(left) > 0 && code == 0 && !g.Helper {
		fmt.Fprintf(os.Stderr, "a test left record files in the package: %v\n", left)
		code = 1
	}
	os.Exit(g.Close(code))
}
