package egress

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sendTo writes raw on a fresh connection to network/addr and returns the
// status line.
func sendTo(t *testing.T, network, addr, raw string) string {
	t.Helper()
	c, err := net.Dial(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, raw)
	line, _ := bufio.NewReader(c).ReadString('\n')
	return line
}

// A connection through a call's own listener is that call's: without
// credentials (as a JVM's CONNECT comes) it is served, and with another
// call's token it is still recorded under the listener's call. Once the call
// ends its listener is gone.
func TestCallListenerBindsTheCall(t *testing.T) {
	p, rec := startProxy(t, Config{})
	a, b := issue(t, p, "call-a"), issue(t, p, "call-b")
	sock := filepath.Join(shortTemp(t), "a.sock")
	if err := a.ListenUnix(sock); err != nil {
		t.Fatal(err)
	}
	if err := b.ListenLoopback(false); err != nil {
		t.Fatal(err)
	}
	if st := sendTo(t, "unix", sock, "GET http://denied.test/nocred HTTP/1.1\r\nHost: denied.test\r\n\r\n"); !strings.Contains(st, " 403 ") {
		t.Fatalf("no credentials on the call's socket: %s", st)
	}
	e := rec.waitFor(t, "the uncredentialed request", func(e Event) bool { return e.Path == "/nocred" })
	if e.CallID != "call-a" {
		t.Fatalf("recorded under %q", e.CallID)
	}
	raw := fmt.Sprintf("GET http://denied.test/swapped HTTP/1.1\r\nHost: denied.test\r\nProxy-Authorization: %s\r\n\r\n", basicFor("x", b))
	if st := sendTo(t, "unix", sock, raw); !strings.Contains(st, " 403 ") {
		t.Fatalf("another call's token: %s", st)
	}
	e = rec.waitFor(t, "the swapped request", func(e Event) bool { return e.Path == "/swapped" })
	if e.CallID != "call-a" {
		t.Fatalf("another call's token moved the traffic to %q", e.CallID)
	}
	if got := b.Addr(); got == p.Addr() || !strings.Contains(b.URL(), got.String()) {
		t.Fatalf("call b's URL %s does not name its own port %s", b.URL(), got)
	}
	if !strings.Contains(strings.Join(b.Env(), "\n"), "JAVA_TOOL_OPTIONS=-Dhttp.proxyHost=127.0.0.1 -Dhttp.proxyPort="+fmt.Sprint(b.Addr().Port())) {
		t.Fatalf("env: %v", b.Env())
	}
	if strings.Contains(strings.Join(issue(t, p, "call-c").Env(), "\n"), "JAVA_TOOL_OPTIONS") {
		t.Fatal("a call with no listener of its own points a JVM at the shared port, which wants credentials")
	}
	if st := sendTo(t, "tcp", b.Addr().String(), "GET http://denied.test/b HTTP/1.1\r\nHost: denied.test\r\n\r\n"); !strings.Contains(st, " 403 ") {
		t.Fatalf("call b's port: %s", st)
	}
	a.End()
	if c, err := net.Dial("unix", sock); err == nil {
		_ = c.Close()
		t.Fatal("an ended call's socket still accepts")
	}
	if err := a.ListenUnix(filepath.Join(filepath.Dir(sock), "late.sock")); err == nil {
		t.Fatal("an ended call was given a listener")
	}
}
