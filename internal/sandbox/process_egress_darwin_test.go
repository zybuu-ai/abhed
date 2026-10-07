package sandbox

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

// Seatbelt can allow only "localhost", which is ::1 as well as 127.0.0.1:
// the proxy holds the port on both, so what a command reaches on [::1] is
// the proxy, never another process.
func TestSeatbeltIPv6LoopbackIsTheProxy(t *testing.T) {
	s, ws := egressProcess(t)
	var rec eventLog
	l := Launch{CallID: "c6", Session: "sess-6", Record: rec.record}
	_ = runLaunched(t, s, ws, l, "true")
	port := s.sessionProxy("sess-6").port()
	out := runLaunched(t, s, ws, l, fmt.Sprintf(`curl -sS -m 10 -o /dev/null -w 'code=%%{http_code}' -x "http://[::1]:%d" http://denied.test/v6`, port))
	want := "code=407" // the proxy, asking for its credentials
	if !ipv6Available() {
		want = "code=000"
	}
	if !strings.Contains(out, want) {
		t.Fatalf("[::1]:%d answered %s, want %s", port, out, want)
	}
}

func ipv6Available() bool {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// Seatbelt cannot redirect DNS, so under the allowlist a macOS command
// resolves nothing itself: raw DNS and the system resolver are both refused.
func TestSeatbeltAllowlistHasNoDNS(t *testing.T) {
	s, ws := egressProcess(t)
	var rec eventLog
	l := Launch{CallID: "cd", Session: "sess-d", Record: rec.record}
	for _, c := range []string{
		// Raw DNS: a UDP socket to any server.
		`/usr/bin/python3 -c 'import socket;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.sendto(b"x",("8.8.8.8",53))'`,
		// The system resolver's socket, which getaddrinfo uses.
		`/usr/bin/python3 -c 'import socket;s=socket.socket(socket.AF_UNIX);s.connect("/var/run/mDNSResponder")'`,
		`/usr/bin/python3 -c 'import socket;socket.getaddrinfo("example.com",443)'`,
	} {
		if out := runLaunched(t, s, ws, l, c+" && echo RESOLVED || echo REFUSED"); !strings.Contains(out, "REFUSED") {
			t.Errorf("ESCAPE: %s worked under the allowlist:\n%s", c, out)
		}
	}
	if out := runLaunched(t, s, ws, l, "dscacheutil -q host -a name example.org"); strings.Contains(out, "address") {
		t.Errorf("ESCAPE: the directory service resolved a name under the allowlist:\n%s", out)
	}
}
