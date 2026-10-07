package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// A process launched as an MCP stdio server reaches an allowed host through
// its proxy, is refused a denied one, and has no direct socket out.
func TestServerCommandConfinesTheNetwork(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	requireNetNS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "server ok "+r.URL.Path)
	}))
	defer srv.Close()
	port := int(netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://")).Port())

	old := egressResolve
	egressResolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "allowed.test" || host == "denied.test" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, fmt.Errorf("no such host %s", host)
	}
	t.Cleanup(func() { egressResolve = old })

	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.Egress = egressPolicy(t, egress.Config{Rules: []egress.Rule{
		{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}})
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	t.Cleanup(func() { _ = s.Close() })

	script := fmt.Sprintf(`curl -sS -m 20 http://allowed.test:%[1]d/allowed; echo
curl -sS -m 20 -o /dev/null -w 'denied=%%{http_code}\n' http://denied.test:%[1]d/
curl -sS -m 5 --noproxy '*' http://127.0.0.1:%[1]d/direct && echo REACHED || echo BLOCKED`, port)
	var log eventLog
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd, err := s.ServerCommand(ctx, "probe", []string{"/bin/sh", "-c", script}, []string{"PATH=/usr/bin:/bin"}, log.record)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := cmd.CombinedOutput()
	got := string(out)
	if !strings.Contains(got, "server ok /allowed") {
		t.Fatalf("the allowed host was not reached:\n%s", got)
	}
	if !strings.Contains(got, "denied=403") {
		t.Fatalf("the denied host was not refused:\n%s", got)
	}
	if !strings.Contains(got, "BLOCKED") || strings.Contains(got, "REACHED") {
		t.Fatalf("ESCAPE: a direct socket got out of a confined server:\n%s", got)
	}
	log.waitFor(t, "the allowed request", func(e map[string]any) bool {
		return e["host"] == "allowed.test" && e["decision"] == "allow" && e["call_id"] == ServerKey("probe")
	})
	log.waitFor(t, "the denied request", func(e map[string]any) bool {
		return e["host"] == "denied.test" && e["decision"] == "deny"
	})
}

// Without an egress policy there is nothing to confine a server to.
func TestServerCommandNeedsTheAllowlist(t *testing.T) {
	s := NewProcess(DefaultPolicy(t.TempDir()))
	if _, err := s.ServerCommand(context.Background(), "x", []string{"true"}, nil, nil); err == nil {
		t.Fatal("a server was given a command with no egress policy")
	}
}
