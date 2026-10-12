package app

import (
	"fmt"

	"golang.org/x/sys/windows/svc/eventlog"
)

// writeSysLog writes msg to the Windows Application event log under the
// source abhed-admin, event id 1. An ordinary user cannot clear that log.
func writeSysLog(msg string) error {
	l, err := eventlog.Open(sysLogTag)
	if err != nil {
		return fmt.Errorf("event log: %w", err)
	}
	defer func() { _ = l.Close() }()
	if err := l.Info(1, msg); err != nil {
		return fmt.Errorf("event log: %w", err)
	}
	return nil
}
