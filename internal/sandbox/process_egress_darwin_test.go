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
