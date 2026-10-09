package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/managed"
)

// TestMain gives the tests an empty home, so none reads the developer's own
// ~/.abhed/secrets.json; a test may set its own.
func TestMain(m *testing.M) {
	// No test reads this machine's managed file; a test that needs one sets its own.
	none := filepath.Join(os.TempDir(), fmt.Sprintf("abhed-no-managed-%d", os.Getpid()))
	managed.ConfigFile, managed.AgentsDir = filepath.Join(none, "config.json"), filepath.Join(none, "agents")
	dir, err := os.MkdirTemp("", "abhed-home-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", dir)
	_ = os.Unsetenv("ABHED_SECRETS_FILE")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
