package app

import (
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// /tasks shows a model-written title and a shell's last output line with
// their escapes visible: no clipboard write, screen clear or reversed text.
func TestTasksEscapeUntrustedText(t *testing.T) {
	hostile := "a\x1b]52;c;ZXZpbA==\x07b\x1b[2Jc\u202ed"
	exit := 0
	j := jobRow{ID: "t1", Kind: "shell\x1b[2J", Background: true, Title: "title " + hostile,
		Activity: "out " + hostile, Status: "exited", ExitCode: &exit, Summary: "sum\n" + hostile,
		Started: time.Now()}
	p := newWorkPanel(nil, nil, nil)
	for name, got := range map[string]string{
		"row":      taskLine(ui.Style{}, 1, j, p),
		"activity": sanitizeLine(j.Activity),
		"record":   p.recordText(j),
		"notice":   finishNotice(ui.Style{}, j, time.Second),
	} {
		if strings.ContainsAny(got, "\x1b\x07\u202e") {
			t.Errorf("%s printed a raw control character: %q", name, got)
		}
		if name != "notice" && !strings.Contains(got, "⟨\\e⟩") {
			t.Errorf("%s does not show the escape: %q", name, got)
		}
	}
}

// ACP task cards carry the same text escaped, not only redacted.
func TestACPTaskInfoEscapes(t *testing.T) {
	hostile := "x\x1b[2J\u202ey\nz"
	s := &acpSession{}
	out := s.taskInfo(abhed.TaskInfo{ID: "t1", Kind: agent.KindShell, Status: "running",
		Description: hostile, Command: hostile, LastLine: hostile, Summary: hostile})
	for _, k := range []string{"description", "command", "last_line", "summary"} {
		v, _ := out[k].(string)
		if v == "" || strings.ContainsAny(v, "\x1b\u202e") {
			t.Errorf("%s = %q", k, v)
		}
	}
	if v := out["last_line"].(string); strings.Contains(v, "\n") {
		t.Errorf("last_line keeps a newline: %q", v)
	}
}
