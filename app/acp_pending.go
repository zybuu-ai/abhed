package app

// The contract's later sections, until each lands; every stub refuses or
// does nothing, and the section that replaces it removes it from here.

import (
	"errors"

	"github.com/zybuu-ai/abhed/internal/linediff"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

type taskNote struct{}

type tasker interface {
	Background() []abhed.TaskInfo
	CancelTask(id string) error
}

func (w *reviewWindow) covers(string) bool                   { return false }
func (w *reviewWindow) stopLinger()                          {}
func (s *acpSession) closeWindowLocked()                     {}
func (c *acpConn) windowAnswered(*acpSession, *reviewWindow) {}
func (c *acpConn) taskChanged(*acpSession, string)           {}
func (c *acpConn) taskEvent(*acpSession, abhed.Event)        {}
func (s *acpSession) liveTasks() int                         { return 0 }
func (s *acpSession) waitingAll() int                        { return 0 }

type acpTerminal struct{}

type reviewFile struct{ hunks []linediff.Hunk }

func (c *acpConn) killTerminals(string)                  {}
func (s *acpSession) reviewOf(string) (reviewFile, bool) { return reviewFile{}, false }
func (f reviewFile) status() string                      { return "" }
func (c *acpConn) restoreFrom(*acpSession, int) ([]string, error) {
	return nil, errors.New("undo is not served yet")
}
