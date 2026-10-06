package app

import (
	"fmt"
	"log/syslog"
	"net"
	"time"
)

// journalSocket is journald's native socket.
var journalSocket = "/run/systemd/journal/socket"

// writeSysLog sends msg to the journal through its native socket, which
// adds the sender's uid itself, or to syslog where there is no journald.
// An ordinary user cannot erase either.
func writeSysLog(msg string) error {
	jerr := writeJournal(msg)
	if jerr == nil {
		return nil
	}
	w, err := syslog.New(syslog.LOG_AUTH|syslog.LOG_NOTICE, sysLogTag)
	if err != nil {
		return fmt.Errorf("journal: %w; syslog: %w", jerr, err)
	}
	defer func() { _ = w.Close() }()
	return w.Notice(msg)
}

// writeJournal writes one entry in the journal's native protocol. msg holds
// no newline, so it is sent as a plain field.
func writeJournal(msg string) error {
	conn, err := net.DialTimeout("unixgram", journalSocket, 2*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	// PRIORITY 5 is notice; SYSLOG_FACILITY 4 is auth.
	_, err = conn.Write([]byte("MESSAGE=" + msg + "\nSYSLOG_IDENTIFIER=" + sysLogTag + "\nPRIORITY=5\nSYSLOG_FACILITY=4\n"))
	return err
}
