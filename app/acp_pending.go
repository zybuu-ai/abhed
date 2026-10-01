package app

// The contract's later sections, until each lands; every stub refuses or
// does nothing, and the section that replaces it removes it from here.

import (
	"errors"

	"github.com/zybuu-ai/abhed/internal/linediff"
)

type acpTerminal struct{}

type reviewFile struct{ hunks []linediff.Hunk }

func (c *acpConn) killTerminals(string)                  {}
func (s *acpSession) reviewOf(string) (reviewFile, bool) { return reviewFile{}, false }
func (f reviewFile) status() string                      { return "" }
func (c *acpConn) restoreFrom(*acpSession, int) ([]string, error) {
	return nil, errors.New("undo is not served yet")
}
