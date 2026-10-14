package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/enginecli"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// engineNames are the container engines a session's tier is chosen from.
var engineNames = []string{"docker", "podman", "nerdctl"}

// realEnginesEnv hands helper processes the engines TestMain found.
const realEnginesEnv = "ABHED_TEST_REAL_ENGINES"

var (
	realEngineMu   sync.Mutex
	realEngineRuns []string
)

// keepOffRealEngine hides the host's container engines from the tier's
// choice, so a test's session takes the process tier, and refuses, recording,
// any run of one. Helper processes, whose PATH a test may have set, are
// handed the engines the parent found.
func keepOffRealEngine() {
	if os.Getenv(realEnginesEnv) == "" {
		found := []string{"/"} // never empty, so a helper does not look again
		dirs := append(filepath.SplitList(os.Getenv("PATH")), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/opt/podman/bin")
		for _, d := range dirs {
			for _, n := range engineNames {
				if p, err := filepath.EvalSymlinks(filepath.Join(d, n)); err == nil {
					found = append(found, p)
				}
			}
		}
		_ = os.Setenv(realEnginesEnv, strings.Join(found, string(os.PathListSeparator)))
	}
	real := map[string]bool{}
	for _, p := range filepath.SplitList(os.Getenv(realEnginesEnv)) {
		real[p] = true
	}
	isReal := func(path string) bool {
		p, err := filepath.EvalSymlinks(path)
		return err == nil && real[p]
	}
	sandbox.LookEngine = func(name string) (string, error) {
		p, err := exec.LookPath(name)
		if err == nil && isReal(p) {
			return "", exec.ErrNotFound
		}
		return p, err
	}
	enginecli.Guard = func(path string) error {
		if !isReal(path) {
			return nil
		}
		realEngineMu.Lock()
		realEngineRuns = append(realEngineRuns, path)
		realEngineMu.Unlock()
		return fmt.Errorf("a test ran the host's real container engine %s", path)
	}
}

// realEngineRan reports, on stderr, any run of the real engine refused.
func realEngineRan() bool {
	realEngineMu.Lock()
	defer realEngineMu.Unlock()
	if len(realEngineRuns) > 0 {
		fmt.Fprintf(os.Stderr, "tests ran the host's real container engine %d time(s): %v\n", len(realEngineRuns), realEngineRuns)
		return true
	}
	return false
}
