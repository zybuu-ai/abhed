package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/managed"
)

// TestMain keeps the tests off this machine's managed file; a test that
// needs one sets its own.
func TestMain(m *testing.M) {
	none := filepath.Join(os.TempDir(), fmt.Sprintf("abhed-no-managed-%d", os.Getpid()))
	managed.ConfigFile, managed.AgentsDir = filepath.Join(none, "config.json"), filepath.Join(none, "agents")
	os.Exit(m.Run())
}
