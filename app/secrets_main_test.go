package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain gives the tests an empty home, so none reads the developer's own
// ~/.abhed/secrets.json; a test may set its own. A trust helper process keeps
// the home its parent test prepared, as does a session helper, whose record
// the parent reads afterwards.
func TestMain(m *testing.M) {
	if os.Getenv("ABHED_TRUST_MAIN_ARGS") != "" || os.Getenv("ABHED_SESS_WS") != "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "abhed-home-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", dir)
	_ = os.Unsetenv("ABHED_SECRETS_FILE")
	code := m.Run()
	_ = os.RemoveAll(dir)
	// No test leaves a record or an export in the package's own folder.
	if left, _ := filepath.Glob("*.jsonl"); len(left) > 0 && code == 0 {
		fmt.Fprintf(os.Stderr, "a test left record files in the package: %v\n", left)
		code = 1
	}
	os.Exit(code)
}
