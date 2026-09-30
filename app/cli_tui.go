package app

import (
	"github.com/zybuu-ai/abhed/internal/ui"
)

// setupTerminal gives the input dock what it keeps between sessions: the
// workspace's prompt history.
func setupTerminal(editor *ui.LineReader, workspace string) {
	if !editor.Raw() {
		return
	}
	editor.SetHistory(ui.LoadHistory(ui.HistoryPath(workspace)))
}
