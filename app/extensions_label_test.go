package app

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// The serve banner names an extension that is not running, since every
// session runs without its veto.
func TestServeBannerNamesAnExtensionThatDidNotStart(t *testing.T) {
	cfg := config.Default()
	cfg.Extensions = []config.ExtensionConfig{{Name: "gate", Command: "/nonexistent/abhed-gate"}}
	set := toolset.Build(t.Context(), cfg, toolset.Options{Workspace: t.TempDir(), Parts: toolset.Vetoes})
	defer set.Close()
	line, failed := extensionsLabel(cfg, set)
	if len(failed) != 1 || failed[0] != "gate" || !strings.Contains(line, "NOT RUNNING: gate") {
		t.Fatalf("banner %q, failed %v", line, failed)
	}
}
