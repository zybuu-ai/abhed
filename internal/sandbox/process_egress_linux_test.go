package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// An allowed name resolves to a synthetic address the proxy maps back, any other fails
// and is recorded, the command holds no capability (as root too), and UDP has no way out.
func TestEgressResolverInTheSandbox(t *testing.T) {
	requireNetNS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "reached "+r.Host)
	}))
	defer srv.Close()
	port := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://")).Port()
	old := egressResolve
	egressResolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "allowed.test" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, fmt.Errorf("no such host %s", host)
	}
	t.Cleanup(func() { egressResolve = old })
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.Egress = egressPolicy(t, egress.Config{Rules: []egress.Rule{
		{Host: "allowed.test", Ports: []int{int(port)}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}})
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	t.Cleanup(func() { _ = s.Close() })
	exe, _ := os.Executable()
	if !s.bwrapDNSOK(exe) {
		t.Skip("bwrap cannot grant the relay the resolver's port here")
	}
	var rec eventLog
	l := Launch{CallID: "call-dns", Session: "sess-dns", Record: rec.record}

	out := runLaunched(t, s, ws, l, "getent hosts allowed.test")
	f := strings.Fields(out)
	if len(f) < 2 || f[1] != "allowed.test" {
		t.Fatalf("getent hosts allowed.test:\n%s", out)
	}
	syn, err := netip.ParseAddr(f[0])
	if err != nil || !egress.SynthPrefix.Contains(syn) {
		t.Fatalf("allowed.test resolved to %q, not a synthetic address", f[0])
	}
	if out := runLaunched(t, s, ws, l, "getent hosts other.test; echo rc=$?"); !strings.Contains(out, "rc=2") {
		t.Fatalf("getent hosts other.test:\n%s", out)
	}
	ev := rec.waitFor(t, "other.test refused", func(e map[string]any) bool {
		return e["kind"] == "dns" && e["host"] == "other.test"
	})
	if ev["decision"] != "deny" || ev["call_id"] != "call-dns" {
		t.Fatalf("other.test recorded as %v", ev)
	}
	// The synthetic address reaches the server as allowed.test, through the proxy.
	get := fmt.Sprintf(`a=$(getent hosts allowed.test | cut -d' ' -f1); curl -sS -m 20 "http://$a:%d/"`, port)
	if out := runLaunched(t, s, ws, l, get); !strings.Contains(out, fmt.Sprintf("reached allowed.test:%d", port)) {
		t.Fatalf("plain request to the synthetic address:\n%s", out)
	}
	sets := "Inh|Prm|Eff|Amb"
	if os.Getuid() == 0 {
		sets += "|Bnd" // root regains its bounding set at exec
	}
	out = runLaunched(t, s, ws, l, "grep -E '^Cap("+sets+")' /proc/self/status; cat /etc/resolv.conf")
	if n := strings.Count(out, "Cap"); n != strings.Count(sets, "|")+1 {
		t.Fatalf("read %d capability sets, want %d:\n%s", n, strings.Count(sets, "|")+1, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Cap") && !strings.HasSuffix(line, "0000000000000000") {
			t.Errorf("the command holds a capability: %s", line)
		}
	}
	if !strings.Contains(out, "nameserver 127.0.0.1") {
		t.Errorf("resolv.conf is not the resolver's:\n%s", out)
	}
	// The command runs as this user, and what it writes is this user's.
	if out := runLaunched(t, s, ws, l, "id -u; touch owned && stat -c %u owned"); strings.Fields(out) == nil ||
		strings.Join(strings.Fields(out), " ") != fmt.Sprintf("%d %d", os.Getuid(), os.Getuid()) {
		t.Errorf("the command's uid and its file's owner: %q, want %d", out, os.Getuid())
	}
	// Plain UDP other than the resolver, QUIC included, has no route.
	udp := `timeout 5 bash -c 'echo x > /dev/udp/1.1.1.1/443' && echo SENT || echo BLOCKED`
	if out := runLaunched(t, s, ws, l, udp); !strings.Contains(out, "BLOCKED") {
		t.Fatalf("ESCAPE: UDP left the sandbox:\n%s", out)
	}
}
