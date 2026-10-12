package sandbox

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os/exec"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// With inspect_tls a command's curl trusts the session CA it is given, and
// a path-narrowed rule allows that path over HTTPS and refuses another.
func TestEgressInspectsACommandsHTTPS(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	requireNetNS(t)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "inside "+r.URL.Path)
	}))
	defer upstream.Close()
	port := netip.MustParseAddrPort(strings.TrimPrefix(upstream.URL, "https://")).Port()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	old := egressStart
	egressStart = func(o egress.Options) (*egress.Proxy, error) { o.UpstreamRoots = roots; return old(o) }
	t.Cleanup(func() { egressStart = old })
	oldResolve := egressResolve
	egressResolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "example.com" { // a name the test server's certificate holds
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, fmt.Errorf("no such host %s", host)
	}
	t.Cleanup(func() { egressResolve = oldResolve })
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.Egress = egressPolicy(t, egress.Config{InspectTLS: true, Rules: []egress.Rule{{Host: "example.com", Ports: []int{int(port)},
		Paths: []string{"/ok"}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}})
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	t.Cleanup(func() { _ = s.Close() })
	var rec eventLog
	l := Launch{CallID: "c-i", Session: "sess-I", Record: rec.record}
	out := runLaunched(t, s, ws, l, fmt.Sprintf(`curl -sS -m 20 https://example.com:%d/ok; echo; curl -sS -m 20 -o /dev/null -w 'code=%%{http_code}' https://example.com:%d/exfil`, port, port))
	if !strings.Contains(out, "inside /ok") || !strings.Contains(out, "code=403") || strings.Contains(out, "inside /exfil") {
		t.Fatalf("output:\n%s", out)
	}
	rec.waitFor(t, "the refused path", func(e map[string]any) bool { return e["path"] == "/exfil" && e["decision"] == "deny" })
}
