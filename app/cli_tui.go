package app

import (
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
