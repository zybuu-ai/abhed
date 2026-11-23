package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Approved plugins ship scripts their hooks and servers run: the profile lets
// commands read ~/.abhed/plugins, never write it, and keeps the decisions
// file beside it unreadable.
func TestSeatbeltReadsPluginsNeverWrites(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	prof := NewProcess(Policy{Workspace: t.TempDir()}).seatbeltProfile()
	for _, st := range PathForms(filepath.Join(home, stateDir)) {
		plugins := filepath.Join(st, "plugins")
		denyState := strings.Index(prof, fmt.Sprintf("(deny file-read* (subpath %q))", st))
		allow := strings.Index(prof, fmt.Sprintf("(allow file-read* (subpath %q))", plugins))
		if denyState < 0 || allow < denyState {
			t.Fatalf("plugins read allow %d must follow the state deny %d in\n%s", allow, denyState, prof)
		}
		if strings.Contains(prof, fmt.Sprintf("(allow file-write* (subpath %q))", plugins)) {
			t.Fatal("commands may write the plugins folder")
		}
		if strings.Contains(prof, fmt.Sprintf("(allow file-read* (subpath %q))", filepath.Join(st, "plugins.json"))) {
			t.Fatal("commands may read the plugin decisions")
		}
	}
}
