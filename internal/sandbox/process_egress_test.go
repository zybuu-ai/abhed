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
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

func egressPolicy(t *testing.T, c egress.Config) *egress.Policy {
	t.Helper()
	p, err := egress.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Every tier but the process tier refuses the allowlist, so the network is
// never opened in its place.
func TestEgressAllowlistRefusedOutsideTheProcessTier(t *testing.T) {
	p := DefaultPolicy(t.TempDir())
	p.Egress = egressPolicy(t, egress.Config{})
	for _, sb := range []Sandbox{NewNone(p), NewContainer(p), NewGVisor(p), NewFence(p)} {
		ok, why := sb.Available()
		if ok || !strings.Contains(why, "allowlist") {
			t.Errorf("%s: available %v, %q", sb.Tier(), ok, why)
		}
	}
	p.MinTier = TierNone
	sb, err := Select(p)
	if err == nil && sb.Tier() != TierProcess {
		t.Fatalf("Select chose %s under the allowlist", sb.Tier())
	}
	p.Tier = TierFence
	if _, err := Select(p); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("the fence was chosen under the allowlist: %v", err)
	}
}

// A command under the process tier reaches an allowed local server through
// the session's proxy, plain and through CONNECT; a host no rule allows is
// refused; a direct socket does not get out; and each decision is recorded
// against the call.
func TestEgressAllowlistThroughTheProcessTier(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	requireNetNS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "egress ok "+r.URL.Path)
	}))
	defer srv.Close()
	srvAddr := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://"))
	port := int(srvAddr.Port())

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

	var mu sync.Mutex
	var events []map[string]any
	rec := func(name string, payload map[string]any) error { //nolint:unparam // the record's signature
		if name != EvEgressDecision {
			t.Errorf("event %s", name)
		}
		mu.Lock()
		events = append(events, payload)
		mu.Unlock()
		return nil
	}
	run := func(command string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		ctx = WithLaunch(ctx, Launch{CallID: "call-eg", Record: rec})
		out, _ := s.Command(ctx, ws, command).CombinedOutput()
		return string(out)
	}

	if out := run(fmt.Sprintf("curl -sS -m 20 http://allowed.test:%d/plain", port)); !strings.Contains(out, "egress ok /plain") {
		t.Fatalf("allowed plain request:\n%s", out)
	}
	if out := run(fmt.Sprintf("curl -sS -m 20 -p http://allowed.test:%d/tunnel", port)); !strings.Contains(out, "egress ok /tunnel") {
		t.Fatalf("allowed CONNECT:\n%s", out)
	}
	if out := run(fmt.Sprintf("curl -sS -m 20 -o /dev/null -w 'code=%%{http_code}' http://denied.test:%d/", port)); !strings.Contains(out, "code=403") {
		t.Fatalf("denied plain request:\n%s", out)
	}
	if out := run(fmt.Sprintf("curl -sS -m 20 -p http://denied.test:%d/ && echo REACHED || echo REFUSED", port)); !strings.Contains(out, "REFUSED") {
		t.Fatalf("denied CONNECT:\n%s", out)
	}
	// Around the proxy: no route out, to the server or anywhere.
	direct := fmt.Sprintf("curl -sS -m 5 --noproxy '*' http://127.0.0.1:%d/direct && echo REACHED || echo BLOCKED", port)
	if out := run(direct); !strings.Contains(out, "BLOCKED") || strings.Contains(out, "REACHED") {
		t.Fatalf("ESCAPE: a direct socket got out under the allowlist:\n%s", out)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := map[string]string{}
		for _, e := range events {
			if e["call_id"] != "call-eg" {
				t.Errorf("event without the call id: %v", e)
			}
			got[fmt.Sprint(e["host"], " ", e["kind"])] = fmt.Sprint(e["decision"])
		}
		mu.Unlock()
		want := map[string]string{"allowed.test http": "allow", "allowed.test connect": "allow",
			"denied.test http": "deny", "denied.test connect": "deny"}
		ok := true
		for k, v := range want {
			if got[k] != v {
				ok = false
			}
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("records: %v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(s.Describe(), "egress proxy") {
		t.Errorf("Describe does not name the proxy: %s", s.Describe())
	}
}
