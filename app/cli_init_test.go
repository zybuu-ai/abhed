package app

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// /init sends its request as the next turn and records /init, not the text.
func TestInitSendsItsRequest(t *testing.T) {
	st, store, _ := customRig(t)
	typeLine(t, st, "/init keep it brief")
	turn := st.takeTurn()
	if turn == nil || !strings.HasPrefix(turn.msg.Text, "Write ABHED.md") || !strings.HasSuffix(turn.msg.Text, "The person adds: keep it brief") {
		t.Fatalf("turn: %+v", turn)
	}
	ev := storeEventsOf(t, store, agent.EvCommandInvoked)
	var p agent.CommandInvoked
	if len(ev) == 1 {
		_ = json.Unmarshal(ev[0].Payload, &p)
	}
	if p.Name != "/init" || p.Source != "builtin" {
		t.Fatalf("command.invoked %+v", p)
	}
}

// The prompt names no memory file but Abhed's own and the README, and no
// other tool's rules file: shipped text names no other product.
func TestInitPromptNamesNoOtherProduct(t *testing.T) {
	for _, f := range regexp.MustCompile(`[\w.-]+\.md\b`).FindAllString(initPrompt, -1) {
		if f != "ABHED.md" && f != "README.md" {
			t.Errorf("the /init prompt names %s", f)
		}
	}
	if regexp.MustCompile(`(?i)\.\w*rules\b|instructions\.md`).MatchString(initPrompt) {
		t.Error("the /init prompt names another tool's files")
	}
}
