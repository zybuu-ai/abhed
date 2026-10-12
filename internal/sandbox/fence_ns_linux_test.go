package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// In mount_namespace mode a command runs in a PID namespace of its own, as
// init's child: it sees only its own processes and cannot name Abhed.
func TestFencePIDNamespace(t *testing.T) {
	f, p := fenced(t, nil)
	if f.Mode() != FenceModeMounts {
		t.Skip("mount_namespace mode is not available here")
	}
	out, err := fenceRun(t, f, p.Workspace, fmt.Sprintf(`echo "ppid=$PPID"; ls /proc | grep -c '^[0-9][0-9]*$'; kill -0 %d 2>/dev/null && echo SEES-ABHED || echo NO-ABHED`, os.Getpid()), nil, "call-pid")
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	lines := strings.Fields(out)
	if len(lines) < 3 || lines[0] != "ppid=1" {
		t.Fatalf("not init's child in a namespace of its own: %s", out)
	}
	if n, _ := strconv.Atoi(lines[1]); n > 5 {
		t.Errorf("/proc shows %d processes", n)
	}
	if !strings.Contains(out, "NO-ABHED") {
		t.Errorf("the command reached Abhed's pid: %s", out)
	}
}

// A signal sent to the launcher, as a stop does, reaches the command, which
// as init's child keeps its default dispositions; Ctrl-C and TERM work.
func TestFenceInitPassesSignals(t *testing.T) {
	f, p := fenced(t, nil)
	if f.Mode() != FenceModeMounts {
		t.Skip("mount_namespace mode is not available here")
	}
	ctx, cancel := context.WithTimeout(WithLaunch(context.Background(), Launch{CallID: "call-sig"}), 60*time.Second)
	defer cancel()
	cmd := f.Command(ctx, p.Workspace, `trap 'echo got-term; exit 7' TERM; echo ready; sleep 30 & wait`)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && !strings.Contains(out.String(), "ready"); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 || !strings.Contains(out.String(), "got-term") {
		t.Fatalf("the command did not take the signal: %v %q", err, out.String())
	}
}

// Under the allowlist a command reaches only the proxy: via the netns relay
// in mount_namespace mode, or by its port in landlock_only with consent.
func TestFenceEgressThroughTheRelay(t *testing.T) {
	t.Run("mount_namespace", func(t *testing.T) { fenceEgressCheck(t, false) })
	t.Run("landlock_only", func(t *testing.T) { fenceEgressCheck(t, true) })
}

func fenceEgressCheck(t *testing.T, portOnly bool) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "egress ok "+r.URL.Path)
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
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{
		{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f, p := fenced(t, func(p *Policy) {
		p.Egress = pol
		p.fenceNoMounts, p.FenceEgressPortOnly = portOnly, portOnly
	})
	if (f.Mode() == FenceModeMounts) == portOnly {
		t.Skipf("the fence is in mode %s", f.Mode())
	}
	run := func(command string) string {
		out, _ := fenceRun(t, f, p.Workspace, command, nil, "call-eg")
		return out
	}
	if out := run(fmt.Sprintf("curl -sS -m 20 http://allowed.test:%d/plain", port)); !strings.Contains(out, "egress ok /plain") {
		t.Fatalf("allowed plain request:\n%s", out)
	}
	if out := run(fmt.Sprintf("curl -sS -m 20 -p http://allowed.test:%d/tunnel", port)); !strings.Contains(out, "egress ok /tunnel") {
		t.Fatalf("allowed CONNECT:\n%s", out)
	}
	if out := run(fmt.Sprintf("curl -sS -m 20 -o /dev/null -w 'code=%%{http_code}' http://denied.test:%d/", port)); !strings.Contains(out, "code=403") {
		t.Fatalf("denied request:\n%s", out)
	}
	direct := fmt.Sprintf("curl -sS -m 5 --noproxy '*' http://127.0.0.1:%d/direct && echo REACHED || echo BLOCKED", port)
	if out := run(direct); !strings.Contains(out, "BLOCKED") || strings.Contains(out, "REACHED") {
		t.Fatalf("ESCAPE: a direct socket got out under the allowlist:\n%s", out)
	}
	if out := run("exec 3<>/dev/udp/1.1.1.1/53 && echo UDP-OPEN || echo UDP-REFUSED"); !strings.Contains(out, "UDP-REFUSED") {
		t.Fatalf("a UDP socket was made under the allowlist:\n%s", out)
	}
	// Each call has its own way in: a live call's token, read from the
	// workspace, does not move another call's traffic to it, and a request
	// with no credentials is the sending call's.
	var rec eventLog
	live, keep := context.WithCancel(context.Background())
	defer keep()
	if out, err := f.Command(WithLaunch(live, Launch{CallID: "call-x", Record: rec.record}), p.Workspace, `printf %s "$HTTP_PROXY" > tok`).CombinedOutput(); err != nil {
		t.Fatalf("x: %v %s", err, out)
	}
	_, _ = fenceRun(t, f, p.Workspace, fmt.Sprintf(`curl -sS -m 10 -o /dev/null -x "$(cat tok)" http://denied.test:%d/borrowed; `+
		`curl -sS -m 10 -o /dev/null -x "http://127.0.0.1:${HTTP_PROXY##*:}" http://denied.test:%d/nocred`, port, port), rec.record, "call-y")
	if e := rec.waitFor(t, "the request without credentials", func(e map[string]any) bool { return e["path"] == "/nocred" }); e["call_id"] != "call-y" {
		t.Fatalf("a request without credentials was recorded as %v", e)
	}
	time.Sleep(300 * time.Millisecond)
	for _, e := range rec.find(func(e map[string]any) bool { return e["path"] == "/borrowed" }) {
		if e["call_id"] == "call-x" {
			t.Fatalf("a borrowed token moved y's traffic to x: %v", e)
		}
	}
}
