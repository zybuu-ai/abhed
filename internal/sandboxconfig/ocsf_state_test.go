package sandboxconfig

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// A managed record.ocsf.path's folder is state: commands can neither read
// nor rewrite the record's OCSF copy, and it may not sit in the workspace.
func TestOCSFFolderIsState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	cfg := config.Default()
	cfg.Record.OCSF = config.OCSFConfig{Enabled: true, Path: "/var/log/abhed/ocsf/events.jsonl"}
	if got := StatePaths(cfg, ws); !slices.Contains(got, "/var/log/abhed/ocsf") {
		t.Fatalf("the OCSF folder is not among the state paths %v", got)
	}
	cfg.Record.OCSF.Path = filepath.Join(ws, "logs", "events.jsonl")
	if err := CheckStatePaths(cfg, ws); err == nil || !strings.Contains(err.Error(), "logs") {
		t.Fatalf("an OCSF file in the workspace: %v", err)
	}
}
