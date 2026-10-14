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
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// A stdio server under the fence gets the process tier's confinement: none
// of the home or Abhed's state but its own HOME and its install folder, the
// workspace writable with its .abhed hidden, and its launch recorded.
func TestFenceServerFilesAreConfined(t *testing.T) {
	for _, mode := range []func(*Policy){nil, noMounts} {
		fenceServerFilesAreConfined(t, mode)
	}
}

func fenceServerFilesAreConfined(t *testing.T, mode func(*Policy)) {
	f, p := fenced(t, mode)
	probe := newServerProbe(t, p.Workspace, false)
	ev := &events{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd, err := f.ServerCommand(ctx, "files", probe.argv, os.Environ(), ev.record(nil))
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the server did not run: %v\n%s", err, out)
	}
	probe.check(t, "files", string(out))
	if l := ev.of("process.launched"); len(l) != 1 || l[0]["call_id"] != ServerKey("files") {
		t.Fatalf("the server's launch was not recorded as its own: %v", l)
	}
	if _, err := os.Stat(filepath.Join(probe.home, ".abhed", "mcp", "files", "home", "tmp")); err != nil {
		t.Fatalf("no temp of its own: %v", err)
	}
}

// Under the allowlist a fenced server reaches an allowed host through a proxy
// of its own, is refused a denied one, and has no direct socket out.
func TestFenceServerUnderTheAllowlist(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
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
	f, p := fenced(t, func(p *Policy) {
		p.Egress = egressPolicy(t, egress.Config{Rules: []egress.Rule{
			{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
		}})
	})
	probe := newServerProbe(t, p.Workspace, false)
	script := fmt.Sprintf(`curl -sS -m 20 http://allowed.test:%[1]d/allowed; echo
curl -sS -m 20 -o /dev/null -w 'denied=%%{http_code}\n' http://denied.test:%[1]d/
curl -sS -m 5 --noproxy '*' http://127.0.0.1:%[1]d/direct && echo REACHED || echo BLOCKED
cat %[2]q 2>/dev/null; echo end`, port, filepath.Join(probe.home, ".ssh", "id_test"))
	var log eventLog
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd, err := f.ServerCommand(ctx, "probe", []string{"/bin/sh", "-c", script}, []string{"PATH=/usr/bin:/bin"}, log.record)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := cmd.CombinedOutput()
	got := string(out)
	if !strings.Contains(got, "server ok /allowed") || !strings.Contains(got, "denied=403") || !strings.Contains(got, "end") {
		t.Fatalf("the allowlist did not apply through the server's proxy:\n%s", got)
	}
	if !strings.Contains(got, "BLOCKED") || strings.Contains(got, "REACHED") || strings.Contains(got, "PRIVATE-KEY") {
		t.Fatalf("ESCAPE: a direct socket or the home reached from a fenced server:\n%s", got)
	}
	log.waitFor(t, "the allowed request", func(e map[string]any) bool {
		return e["host"] == "allowed.test" && e["decision"] == "allow" && e["call_id"] == ServerKey("probe")
	})
}
