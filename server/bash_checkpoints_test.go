package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// A served session keeps no bash checkpoints, so tools.bash_checkpoints
// "required" refuses its commands rather than running them unrecorded.
func TestServedSessionRefusesCommandsUnderRequiredCheckpoints(t *testing.T) {
	wb := manualBench(t, func(c *config.Config) { c.Tools.BashCheckpoints = "required" })
	rec := wb.send("acme", "POST", "exec", execRequest{Command: "touch ran.txt"})
	if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), "tools.bash_checkpoints") {
		t.Fatalf("exec under required: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "required") {
		t.Fatalf("the refusal does not say why: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(wb.workspace, "ran.txt")); err == nil {
		t.Fatal("the command ran")
	}
	// The default runs it.
	wb = manualBench(t, nil)
	if rec := wb.send("acme", "POST", "exec", execRequest{Command: "touch ran.txt"}); rec.Code != http.StatusOK {
		t.Fatalf("exec: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(wb.workspace, "ran.txt")); err != nil {
		t.Fatal("the command did not run")
	}
}
