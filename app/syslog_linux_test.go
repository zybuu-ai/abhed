package app

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// An entry reaches the journal in its native protocol, under the
// abhed-admin identifier, as a single MESSAGE field.
func TestJournalEntryFields(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "journal.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Skipf("no unix datagram socket: %v", err)
	}
	defer func() { _ = conn.Close() }()
	old := journalSocket
	journalSocket = sock
	t.Cleanup(func() { journalSocket = old })
	msg := sysLogMessage(adminEntry{Time: "t", UID: "1000", Action: "web-search on", Result: "changed", Reason: "a\nb"})
	if err := writeJournal(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(buf[:n]), "\n"), "\n")
	want := []string{"MESSAGE=" + msg, "SYSLOG_IDENTIFIER=abhed-admin", "PRIORITY=5", "SYSLOG_FACILITY=4"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("datagram %q", buf[:n])
	}
}
