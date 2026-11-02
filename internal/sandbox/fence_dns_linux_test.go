package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	// The namespace holds loopback alone and no route, so UDP, which the
	// filter allows here for the resolver, reaches nothing else.
	if out, _ := fenceRun(t, f, p.Workspace, "grep -cv -e '^Inter' -e '^ face' -e '^ *lo:' /proc/net/dev; grep -c . /proc/net/route; awk '$NF != \"lo\"' /proc/net/ipv6_route | wc -l", nil, "call-dns4"); strings.Join(strings.Fields(out), " ") != "0 1 0" {
		t.Fatalf("the namespace has more than loopback:\n%s", out)
	}
	if _, err := exec.LookPath("python3"); err == nil {
		udp := `import socket
for fam, addr in ((socket.AF_INET, "1.1.1.1"), (socket.AF_INET, "10.0.2.3"), (socket.AF_INET6, "2606:4700:4700::1111")):
    s = socket.socket(fam, socket.SOCK_DGRAM)
    try:
        s.sendto(b"x", (addr, 53)); print("SENT", addr)
    except OSError as e:
        print("UNREACHABLE", addr, e.errno)
q = bytes.fromhex("abcd01000001000000000000") + b"\x07allowed\x04test\x00\x00\x01\x00\x01"
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(5); s.sendto(q, ("127.0.0.1", 53))
r = s.recv(512); print("ANSWER", ".".join(str(b) for b in r[-4:]))`
		out, _ := fenceRun(t, f, p.Workspace, "python3 -c '"+udp+"'", nil, "call-dns5")
		t.Logf("UDP:\n%s", out)
		if strings.Contains(out, "SENT") || strings.Count(out, "UNREACHABLE") != 3 || !strings.Contains(out, "ANSWER 198.1") {
			t.Fatalf("UDP under the fence:\n%s", out)
		}
	}
	// A musl resolver, which sends UDP alone, resolves as glibc's does.
	if dir := os.Getenv("ABHED_TEST_MUSL_DIR"); dir != "" {
		g, gp := fenced(t, func(q *Policy) { q.Egress = pol; q.ReadOnlyPaths = append(q.ReadOnlyPaths, dir) })
		bb := dir + "/lib/ld-musl-" + muslArch() + ".so.1 " + dir + "/bin/busybox"
		out, _ := fenceRun(t, g, gp.Workspace, bb+" nslookup allowed.test; "+bb+" nslookup other.test || echo OTHER-NONE", nil, "call-musl")
		t.Logf("busybox (musl) nslookup:\n%s", out)
		if !strings.Contains(out, "198.1") || !strings.Contains(out, "OTHER-NONE") {
			t.Fatalf("busybox (musl) nslookup:\n%s", out)
		}
		// And through musl's getaddrinfo, which wget uses: with the proxy off,
		// the connection to the synthetic address is refused after the lookup.
		out, _ = fenceRun(t, g, gp.Workspace, bb+" wget -Y off -T 2 -O /dev/null http://allowed.test/; "+bb+" wget -Y off -T 2 -O /dev/null http://other.test/", nil, "call-musl2")
		t.Logf("busybox (musl) wget:\n%s", out)
		if !strings.Contains(out, "(198.1") || !strings.Contains(out, "bad address 'other.test'") {
			t.Fatalf("musl's getaddrinfo:\n%s", out)
		}
	}
}

// A static musl program's getaddrinfo, as CI builds it, resolves the same.
func TestFenceResolvesWithMusl(t *testing.T) {
	helper := os.Getenv("ABHED_TEST_MUSL_RESOLVE")
	if helper == "" {
		// Where the fence's tests must all run, a missing helper is a failure.
		if os.Getenv("ABHED_REQUIRE_FENCE") == "1" {
			t.Fatal("ABHED_REQUIRE_FENCE=1 but ABHED_TEST_MUSL_RESOLVE names no static musl resolver helper (scripts/ci/musl-resolve.c, built with musl-gcc -static)")
		}
		t.Skip("set ABHED_TEST_MUSL_RESOLVE to a static musl resolver helper")
	}
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "allowed.test", Ports: []int{80}, Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	f, p := fenced(t, func(p *Policy) { p.Egress = pol; p.ReadOnlyPaths = append(p.ReadOnlyPaths, filepath.Dir(helper)) })
	if f.Mode() != FenceModeMounts {
		t.Skip("mount_namespace mode is not available here")
	}
	out, _ := fenceRun(t, f, p.Workspace, helper+" allowed.test; "+helper+" other.test || echo OTHER-NONE", nil, "call-musl-c")
	t.Logf("musl getaddrinfo:\n%s", out)
	addr, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if a, err := netip.ParseAddr(addr); err != nil || !egress.SynthPrefix.Contains(a) || !strings.Contains(out, "OTHER-NONE") {
		t.Fatalf("musl getaddrinfo under the fence:\n%s", out)
	}
}

// muslArch is musl's name for this machine, as its loader is named.
func muslArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	if runtime.GOARCH == "amd64" {
		return "x86_64"
	}
	return runtime.GOARCH
}
