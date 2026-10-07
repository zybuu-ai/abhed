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
	"runtime"
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
curl -sS -m 5 --noproxy '*' http://127.0.0.1:%[1]d/direct && echo REACHED || echo BLOCKED
echo "bus=${DBUS_SESSION_BUS_ADDRESS:-none} socket=$(test -S /run/user/$(id -u)/bus && echo visible || echo hidden)"`, port)
	var log eventLog
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd, err := s.ServerCommand(ctx, "probe", []string{"/bin/sh", "-c", script},
		[]string{"PATH=/usr/bin:/bin", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1/bus"}, log.record)
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
	if !strings.Contains(got, "bus=none socket=hidden") {
		t.Fatalf("the session bus was handed to the server:\n%s", got)
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

// The macOS profile keeps LaunchServices from the server, which would open a
// URL or an app outside the sandbox for it.
func TestServerProfileDeniesLaunchServices(t *testing.T) {
	p := serverProfile(4000)
	for _, want := range []string{`(deny network*)`, `(remote ip "localhost:4000")`, `"com.apple.coreservices.launchservicesd"`, `"com.apple.lsd."`} {
		if !strings.Contains(p, want) {
			t.Errorf("profile lacks %s:\n%s", want, p)
		}
	}
	env := serverEnv([]string{"PATH=/bin", "DBUS_SESSION_BUS_ADDRESS=x", "SSH_AUTH_SOCK=y", "https_proxy=z", "TOKEN=t"})
	if strings.Join(env, " ") != "PATH=/bin TOKEN=t" {
		t.Errorf("server env %v", env)
	}
}

// Under bubblewrap a root-run Abhed starts no stdio server: it would keep
// root's capabilities over the host.
func TestServerRefusedAsRoot(t *testing.T) {
	for _, ids := range [][2]int{{0, 0}, {1000, 0}, {0, 1000}} {
		if err := serverRootRefusal(ids[0], ids[1]); err == nil || !strings.Contains(err.Error(), "root") {
			t.Errorf("uid %d euid %d: %v", ids[0], ids[1], err)
		}
	}
	if err := serverRootRefusal(1000, 1000); err != nil {
		t.Errorf("an ordinary user was refused: %v", err)
	}
	if os.Geteuid() == 0 && runtime.GOOS == "linux" {
		p := DefaultPolicy(t.TempDir())
		p.Egress = egressPolicy(t, egress.Config{})
		s := NewProcess(p)
		if s.Backend() == "bwrap" {
			if _, err := s.ServerCommand(context.Background(), "x", []string{"true"}, nil, nil); err == nil {
				t.Fatal("a stdio server was given a command as root")
			}
		}
	}
}
