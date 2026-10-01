package server

import (
	"os"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/termline"
)

// An interactive shell is judged line by line by internal/termline, which
// the editor protocol's Abhed terminal shares; these are its names here.

type (
	lineCapture = termline.Capture
	enteredLine = termline.Entered
)

// maxManualCommand bounds a command line typed into the workbench.
const maxManualCommand = termline.MaxLine

func newLineCapture(callID string, record func(agent.TerminalInput)) *lineCapture {
	return termline.NewCapture(callID, record)
}

func plainText(b []byte) string           { return termline.PlainText(b) }
func keepTail(b []byte, n int) []byte     { return termline.KeepTail(b, n) }
func ttyNow(f *os.File) (int, bool, bool) { return termline.TTYNow(f) }
