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

	"github.com/zybuu-ai/abhed/internal/egress"
)

// In mount_namespace mode under the allowlist a command resolves an allowed
// name through the session's resolver, as on the process tier, to an address
// that reaches it through the proxy; any other name resolves to nothing.
func TestFenceResolvesAllowedNames(t *testing.T) {
	if _, err := exec.LookPath("getent"); err != nil {
		t.Skip("getent is not installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "reached "+r.Host)
	}))
	defer srv.Close()
	port := int(netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://")).Port())
	old := egressResolve
	egressResolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "allowed.test" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, fmt.Errorf("no such host %s", host)
	}
	t.Cleanup(func() { egressResolve = old })
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{
		{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f, p := fenced(t, func(p *Policy) { p.Egress = pol })
	if f.Mode() != FenceModeMounts {
		t.Skip("mount_namespace mode is not available here")
	}
	var rec eventLog
	out, _ := fenceRun(t, f, p.Workspace, "getent hosts allowed.test", rec.record, "call-dns")
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[1] != "allowed.test" {
		t.Fatalf("getent hosts allowed.test:\n%s", out)
	}
	if syn, err := netip.ParseAddr(fields[0]); err != nil || !egress.SynthPrefix.Contains(syn) {
		t.Fatalf("allowed.test resolved to %q, not a synthetic address", fields[0])
	}
	rec.waitFor(t, "the lookup's record", func(e map[string]any) bool {
		return e["kind"] == "dns" && e["host"] == "allowed.test" && e["call_id"] == "call-dns"
	})
	if out, _ := fenceRun(t, f, p.Workspace, "getent hosts other.test && echo RESOLVED || echo NONE", nil, "call-dns2"); !strings.Contains(out, "NONE") {
		t.Fatalf("a name no rule allows resolved:\n%s", out)
	}
	// The name reaches the server through the proxy.
	if _, err := exec.LookPath("curl"); err == nil {
		got, _ := fenceRun(t, f, p.Workspace, fmt.Sprintf("curl -sS -m 20 http://allowed.test:%d/", port), nil, "call-dns3")
		if !strings.Contains(got, "reached allowed.test") {
			t.Fatalf("curl through the resolver:\n%s", got)
		}
	}
	// UDP stays refused: the resolver is reached over TCP alone.
	if out, _ := fenceRun(t, f, p.Workspace, "exec 3<>/dev/udp/127.0.0.1/53 && echo UDP-OPEN || echo UDP-REFUSED", nil, "call-dns4"); !strings.Contains(out, "UDP-REFUSED") {
		t.Fatalf("a UDP socket was made under the allowlist:\n%s", out)
	}
}
