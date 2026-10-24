package agent

import (
	"os"
	"strings"
	"testing"
)

// Every event type the CLI records has its row in the data model, so the
// record's readers learn each shape in one place.
func TestCLIEventTypesAreDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/architecture/10-data-model.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []EventType{EvModeChanged, EvPermissionChanged, EvWorkspaceDirAdded, EvInputMention,
		EvCommandInvoked, EvMemoryLoaded, EvMemoryWritten, EvSessionNamed, EvSessionBranched, EvFileRestored,
		EvPlanProposed, EvPlanDecided, EvModelFallback, EvHookFired, EvRecordRepaired, EvSubagentMessage, EvSubagentBackgrounded, EvWorktreeTaken,
		EvMonitorVerdict, EvSupervisorCorrection, EvSupervisorReplan} {
		if !strings.Contains(string(doc), "| `"+string(e)+"` |") {
			t.Errorf("%s has no row in docs/architecture/10-data-model.md", e)
		}
	}
	if !strings.Contains(string(doc), "| `ollama.start`, `ollama.install`, `ollama.pull` |") {
		t.Error("the ollama events have no row in docs/architecture/10-data-model.md")
	}
}
