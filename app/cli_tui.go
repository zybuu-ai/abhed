package app

import (
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// setupTerminal connects the renderer to the input dock, so replies stream
// into it, and gives the dock what it keeps between sessions: the
// workspace's prompt history.
func setupTerminal(editor *ui.LineReader, r *ui.Renderer, workspace string) {
	r.SetWorkspace(workspace)
	if !editor.Raw() {
		return
	}
	r.Attach(editor)
	editor.SetHistory(ui.LoadHistory(ui.HistoryPath(workspace)))
}

// dialogApprover asks on the dock's dialog when there is a terminal, and
// through ap's line prompt otherwise. Files are read through the session,
// under its roots and state protection, for the diff an edit would make.
func dialogApprover(ap *ui.Approver, editor *ui.LineReader, r *ui.Renderer, sess *tools.Session) agent.Approver {
	if !editor.Raw() {
		return ap
	}
	return &ui.DialogApprover{Base: ap, Reader: editor, Render: r, ReadFile: sess.ReadFile}
}
