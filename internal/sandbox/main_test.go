package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/testenv"
)

// The test binary links the managed paths; keep its tests off this
// machine's managed files and model services. Afterwards remove the caches
// and temp folders the tests' sandboxes left (many are never closed).
func TestMain(m *testing.M) {
	g := testenv.Isolate()
	code := m.Run()
	removeTestTemps()
	os.Exit(g.Close(code))
}

// removeTestTemps removes, in the temp areas a process sandbox uses, the
// folders with Abhed's names whose owner file names this process, and those
// whose owner has ended, as a helper this binary ran.
func removeTestTemps() {
	me := ownerOf(os.Getpid())
	for _, parent := range reapParents() {
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !hasReapedPrefix(e.Name()) {
				continue
			}
			dir := filepath.Join(parent, e.Name())
			b, err := os.ReadFile(filepath.Join(dir, tempOwnerFile)) // #nosec G304 -- the owner file of a folder with Abhed's name
			mine := err == nil && me != "" && strings.TrimSpace(string(b)) == me
			if mine || ownerOfTemp(dir) == ownerEnded {
				makeRemovable(dir)
				_ = os.RemoveAll(dir)
			}
		}
	}
}
