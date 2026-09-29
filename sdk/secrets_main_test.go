package abhed

import (
	"os"
	"testing"
)

// TestMain gives the tests an empty home, so none reads the developer's own
// ~/.abhed/secrets.json; a test may set its own.
func TestMain(m *testing.M) {
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
