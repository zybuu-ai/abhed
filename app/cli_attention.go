package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/todos", Help: "the agent's task list as it stands (ctrl+t shows it above the input)",
		Group: "status", Order: 172, MidTurn: true, ReadOnly: true, Run: slashTodos})
	registerSlash(slashCmd{Name: "/copy", Help: "copy the last reply to the clipboard through the terminal",
		Group: "app", Order: 176, MidTurn: true, ReadOnly: true, Run: slashCopy})
}

// slashTodos is /todos: the latest todo list, drawn into the transcript.
func slashTodos(_ context.Context, e *cmdEnv, _ []string) (bool, error) {
	if e.r == nil || !e.r.ShowTodos() {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "no todo list yet: the agent writes one for work with several steps"})
	}
	return false, nil
}

// slashCopy is /copy: the last reply onto the clipboard, by OSC 52. It is
// the only way the clipboard is written, and only the person runs it.
func slashCopy(_ context.Context, e *cmdEnv, _ []string) (bool, error) {
	if !e.st.appCfg.CLI.Copy {
		return false, errors.New("copying is turned off by cli.copy")
	}
	l, ok := e.ui.(*ui.LineReader)
	if !ok {
		return false, errors.New("copying needs a terminal")
	}
	text := ""
	if e.r != nil {
		text = e.r.LastReply()
	}
	if strings.TrimSpace(text) == "" {
		return false, errors.New("there is no reply to copy yet")
	}
	if err := l.Copy(text); err != nil {
		return false, err
	}
	n := strings.Count(strings.TrimRight(text, "\n"), "\n") + 1
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: fmt.Sprintf(
		"sent the last reply (%d line%s) to the terminal's clipboard; a terminal that does not allow OSC 52 ignores it",
		n, map[bool]string{true: "", false: "s"}[n == 1])})
	if e.r.LastReplyCut() {
		e.ui.Notify(ui.Toast{Warn: true, Text: "the reply held hidden or control characters, which the copy leaves out; check it before you paste it anywhere"})
	}
	return false, nil
}
