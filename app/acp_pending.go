package app

// The contract's later sections, until each lands; every stub refuses or
// does nothing, and the section that replaces it removes it from here.

import (
	"encoding/json"
	"errors"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/hawkeye"
	"github.com/zybuu-ai/abhed/internal/linediff"
	"github.com/zybuu-ai/abhed/internal/policy"
	abhed "github.com/zybuu-ai/abhed/sdk"
	"github.com/zybuu-ai/abhed/store/local"
)

type eventSub struct {
	session string
	stop    chan struct{}
}

func (c *acpConn) unsubscribeSession(string)   {}
func (c *acpConn) restartForTrust(*acpSession) {}
func (c *acpConn) recorded(string) (*local.Store, local.Entry, *rpcError) {
	return nil, local.Entry{}, refusal(errNoMethod, "the record's methods are not served yet")
}
func hawkeyeOf(*local.Store, local.Entry) hawkeye.Report { return hawkeye.Report{} }
func writeHawkeyeExport(*local.Store, local.Entry, string) error {
	return errors.New("export is not served yet")
}

func (c *acpConn) modeState(*acpSession) map[string]any          { return nil }
func modeConfigOption(map[string]any) map[string]any             { return nil }
func availableModes(config.Config) []policy.Mode                 { return nil }
func protectedPaths(string) []string                             { return nil }
func (s *acpSession) dirtyGuard(string) error                    { return nil }
func (c *acpConn) checkTrust(*acpSession) bool                   { return false }
func (s *acpSession) askDiff(string, json.RawMessage) *askChange { return nil }
func (c *acpConn) changeMode(*acpSession, string) *rpcError {
	return refusal(errNoMethod, "modes are not served yet")
}

type askChange struct {
	content, locations, meta []any
	hunksOnly                bool
}

func (c *acpConn) configOptions(s *acpSession) []any {
	if m, ok := s.agent.(modelSwitcher); ok {
		if models := m.Models(); len(models) > 0 {
			return modelConfigOptions(models)
		}
	}
	return nil
}

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
