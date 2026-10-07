package egress

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The relay joins a loopback connection to the proxy's unix socket, and the
// proxy decides as on its own port.
func TestRelayReachesTheProxyOverItsSocket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "via relay") }))
	defer srv.Close()
	port := portOf(t, srv.URL)
	p, rec := startProxy(t, Config{Rules: []Rule{{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}})
	dir, err := os.MkdirTemp("", "eg")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "p.sock")
	if err := p.ListenUnix(sock); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go Relay(ln, sock)

	user, pass := creds(t, p)
	c := client(t, fmt.Sprintf("http://%s:%s@%s", user, pass, ln.Addr()), nil)
	resp, err := c.Get(fmt.Sprintf("http://allowed.test:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "via relay" {
		t.Fatalf("body %q", body)
	}
	resp, err = c.Get(fmt.Sprintf("http://denied.test:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied through the relay: %d", resp.StatusCode)
	}
	if ev := rec.wait(t, "allowed.test"); ev.Decision != Allow {
		t.Fatalf("%+v", ev)
	}
}

func TestRunRelayUsage(t *testing.T) {
	if RunRelay([]string{"sock"}) != 126 || RunRelay([]string{"a", "b", "c", "d"}) != 126 {
		t.Fatal("a malformed relay command ran")
	}
}
