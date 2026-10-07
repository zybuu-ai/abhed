package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// eventLog collects one record's egress decisions.
type eventLog struct {
	mu  sync.Mutex
	evs []map[string]any
}

func (l *eventLog) record(name string, payload map[string]any) error {
	if name == EvEgressDecision {
		l.mu.Lock()
		l.evs = append(l.evs, payload)
		l.mu.Unlock()
	}
	return nil
}

func (l *eventLog) find(f func(map[string]any) bool) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, e := range l.evs {
		if f(e) {
			out = append(out, e)
		}
	}
	return out
}

func (l *eventLog) waitFor(t *testing.T, what string, f func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := l.find(f); len(got) > 0 {
			return got[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no event: %s; have %v", what, l.find(func(map[string]any) bool { return true }))
	return nil
}

// egressProcess is a process tier under an allowlist that denies
// denied.test, with the tier skipped where it cannot run.
func egressProcess(t *testing.T) (*Process, string) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	requireNetNS(t)
	old := egressResolve
	egressResolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		return nil, fmt.Errorf("no such host %s", host)
	}
	t.Cleanup(func() { egressResolve = old })
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.Egress = egressPolicy(t, egress.Config{Rules: []egress.Rule{{Host: "denied.test", Decision: "deny"}}})
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, ws
}

func runLaunched(t *testing.T, s *Process, ws string, l Launch, command string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, _ := s.Command(WithLaunch(ctx, l), ws, command).CombinedOutput()
	return string(out)
}

func (s *Process) sessionProxy(id string) *egressState {
	s.egress.mu.Lock()
	defer s.egress.mu.Unlock()
	return s.egress.m[id]
}

// Two sessions of one server: each has its own proxy, a call id a command
// claims is ignored for its credential's, and a terminal is its own call.
func TestEgressSessionsAreKeptApart(t *testing.T) {
	s, ws := egressProcess(t)
	var recA, recB eventLog
	a := Launch{CallID: "call-a", Session: "sess-A", Record: recA.record}
	b := Launch{CallID: "call-b", Session: "sess-B", Record: recB.record}

	_ = runLaunched(t, s, ws, b, "curl -sS -m 20 -o /dev/null http://denied.test/b")
	recB.waitFor(t, "B's own request", func(e map[string]any) bool { return e["call_id"] == "call-b" })
	_ = runLaunched(t, s, ws, a, "curl -sS -m 20 -o /dev/null http://denied.test/a")
	recA.waitFor(t, "A's own request", func(e map[string]any) bool { return e["call_id"] == "call-a" })

	pa, pb := s.sessionProxy("sess-A").proxy, s.sessionProxy("sess-B").proxy
	if pa == pb || pa.Addr() == pb.Addr() {
		t.Fatal("the two sessions share a proxy or a token")
	}

	// A claims B's call id in its proxy credentials.
	forge := `curl -sS -m 20 -o /dev/null -x "$(printf %s "$HTTP_PROXY" | sed 's#//call-a:#//call-b:#')" http://denied.test/forged`
	_ = runLaunched(t, s, ws, a, forge)
	got := recA.waitFor(t, "the forged request in A's record", func(e map[string]any) bool { return e["path"] == "/forged" })
	if got["call_id"] != "call-a" {
		t.Fatalf("forged request recorded as %v, want under its credential's call-a", got)
	}
	// Another call's credential, taken after that call ended, is refused and
	// recorded under the ended call.
	var recA2 eventLog
	a2 := Launch{CallID: "call-a2", Session: "sess-A", Record: recA2.record}
	stolen := strings.TrimSpace(runLaunched(t, s, ws, a2, `printf %s "$HTTP_PROXY"`))
	late := fmt.Sprintf(`curl -sS -m 20 -o /dev/null -w 'code=%%{http_code}' -x '%s' http://denied.test/stolen`, stolen)
	if out := runLaunched(t, s, ws, a, late); !strings.Contains(out, "code=407") {
		t.Fatalf("an ended call's credential was accepted: %s", out)
	}
	ended := recA2.waitFor(t, "the stolen credential's refusal", func(e map[string]any) bool { return e["kind"] == "auth" })
	if ended["call_id"] != "call-a2" || ended["decision"] != "deny" {
		t.Fatalf("stolen credential recorded as %v", ended)
	}
	// B's proxy, reached directly with A's token, refuses it.
	cross := fmt.Sprintf(`curl -sS -m 20 -o /dev/null -w 'code=%%{http_code}' -x "$(printf %%s "$HTTP_PROXY" | sed 's#:[0-9]*$#:%d#')" http://denied.test/cross`, pb.Addr().Port())
	out := runLaunched(t, s, ws, a, cross)
	if strings.Contains(out, "code=403") {
		t.Fatalf("A's command reached B's proxy and was decided: %s", out)
	}

	// The terminal, a long-lived shell bound to session A as its own call.
	term := Launch{CallID: "term-1", Session: "sess-A", Record: recA.record}
	ctx, cancel := context.WithTimeout(WithLaunch(context.Background(), term), 60*time.Second)
	defer cancel()
	sh := s.Shell(ctx, ws)
	sh.Stdin = strings.NewReader("curl -sS -m 20 -o /dev/null http://denied.test/terminal\nexit\n")
	if out, err := sh.CombinedOutput(); err != nil && !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("terminal: %v\n%s", err, out)
	}
	tev := recA.waitFor(t, "the terminal's request", func(e map[string]any) bool { return e["path"] == "/terminal" })
	if tev["call_id"] != "term-1" {
		t.Fatalf("terminal recorded as %v", tev)
	}

	time.Sleep(200 * time.Millisecond)
	for _, e := range recB.find(func(map[string]any) bool { return true }) {
		if e["call_id"] != "call-b" {
			t.Errorf("B's record holds another session's decision: %v", e)
		}
	}
	for _, e := range recA.find(func(map[string]any) bool { return true }) {
		if e["path"] == "/b" {
			t.Errorf("A's record holds B's decision: %v", e)
		}
	}

	// Ending session B stops its proxy and removes its socket's folder; A's
	// keeps running, and B's next command gets a new proxy and token.
	dirB := s.sessionProxy("sess-B").dir
	if err := s.EndSession("sess-B"); err != nil {
		t.Fatal(err)
	}
	if dirB != "" {
		if _, err := os.Stat(dirB); !os.IsNotExist(err) {
			t.Fatalf("B's socket folder is left: %v", err)
		}
	}
	if c, err := net.Dial("tcp", pb.Addr().String()); err == nil {
		_ = c.Close()
		t.Fatal("B's proxy still listens after its session ended")
	}
	_ = runLaunched(t, s, ws, a, "curl -sS -m 20 -o /dev/null http://denied.test/after")
	recA.waitFor(t, "A after B ended", func(e map[string]any) bool { return e["path"] == "/after" })
	_ = runLaunched(t, s, ws, b, "curl -sS -m 20 -o /dev/null http://denied.test/b2")
	recB.waitFor(t, "B's next proxy", func(e map[string]any) bool { return e["path"] == "/b2" })
	if s.sessionProxy("sess-B").proxy == pb {
		t.Fatal("B kept its old proxy")
	}
}

// A decision with no record to go to, from a command run outside any
// session, is dropped, never written to a session's record.
func TestEgressWithoutARecordIsDropped(t *testing.T) {
	s, ws := egressProcess(t)
	var rec eventLog
	_ = runLaunched(t, s, ws, Launch{CallID: "c", Session: "sess-A", Record: rec.record}, "true")
	_ = runLaunched(t, s, ws, Launch{}, "curl -sS -m 20 -o /dev/null http://denied.test/orphan")
	time.Sleep(300 * time.Millisecond)
	if got := rec.find(func(e map[string]any) bool { return e["path"] == "/orphan" }); len(got) > 0 {
		t.Fatalf("a session-less command's decision reached a session: %v", got)
	}
}

// After Close, a command is refused rather than given a proxy that is
// gone, and every proxy's socket folder is removed.
func TestEgressRefusedAfterClose(t *testing.T) {
	s, ws := egressProcess(t)
	var rec eventLog
	l := Launch{CallID: "c", Session: "sess-A", Record: rec.record}
	_ = runLaunched(t, s, ws, l, "true")
	dir := s.sessionProxy("sess-A").dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if dir != "" {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("socket folder left: %v", err)
		}
	}
	marker := filepath.Join(ws, "ran-after-close")
	cmd := s.Command(WithLaunch(context.Background(), l), ws, "touch "+marker)
	if err := cmd.Run(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("a command ran after Close: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran")
	}
}

// A proxy that cannot start refuses the command, never the open network.
func TestEgressFailsClosedWhenTheProxyCannotStart(t *testing.T) {
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.Egress = egressPolicy(t, egress.Config{Default: "allow"})
	s := NewProcess(p)
	t.Cleanup(func() { _ = s.Close() })
	old := egressStart
	egressStart = func(egress.Options) (*egress.Proxy, error) { return nil, errors.New("no loopback today") }
	t.Cleanup(func() { egressStart = old })
	marker := filepath.Join(ws, "ran")
	for _, cmd := range []*exec.Cmd{
		s.Command(context.Background(), ws, "touch "+marker),
		s.Shell(context.Background(), ws),
	} {
		if cmd.Err == nil || !strings.Contains(cmd.Err.Error(), "no loopback today") {
			t.Fatalf("command built without its proxy: %v %v", cmd.Args, cmd.Err)
		}
		if err := cmd.Run(); err == nil {
			t.Fatal("the command ran")
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran")
	}
}

// Under the allowlist the Seatbelt profile denies the network and allows
// only the session's proxy port on localhost, after the deny; without a
// proxy, or without the allowlist, no port is allowed.
func TestSeatbeltEgressProfile(t *testing.T) {
	p := DefaultPolicy("/w")
	p.Egress = egressPolicy(t, egress.Config{})
	s := NewProcess(p)
	prof := s.seatbeltProfileFor(4321)
	deny := strings.Index(prof, "(deny network*)")
	allow := strings.Index(prof, `(allow network-outbound (remote ip "localhost:4321"))`)
	if deny < 0 || allow < deny {
		t.Fatalf("profile:\n%s", prof)
	}
	if strings.Count(prof, "allow network") != 1 {
		t.Fatalf("more than the proxy is allowed:\n%s", prof)
	}
	if strings.Contains(s.seatbeltProfileFor(0), "allow network") {
		t.Fatal("a port was allowed with no proxy")
	}
	open := DefaultPolicy("/w")
	open.AllowNetwork = true
	if strings.Contains(NewProcess(open).seatbeltProfileFor(4321), "(deny network*)") {
		t.Fatal("the open network was denied")
	}
	if s.wrapEgress(context.Background(), "/w", nil, nil, "/bin/true").Err == nil {
		t.Fatal("a command was built under the allowlist with no proxy")
	}
}

// proxyOf is the address the command's proxy variables name.
func proxyOf(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	if cmd.Err != nil {
		t.Fatalf("command not built: %v", cmd.Err)
	}
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "HTTP_PROXY="); ok {
			u, err := url.Parse(v)
			if err != nil {
				t.Fatal(err)
			}
			return u.Host
		}
	}
	t.Fatalf("no proxy in the command's environment")
	return ""
}

func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// A session's proxy stays while a command is in flight, closes when quiet,
// and the next command opens a new one with a new token.
func TestEgressProxyClosesWhenQuiet(t *testing.T) {
	s, ws := egressProcess(t)
	s.egress.quiet = 100 * time.Millisecond
	l := Launch{CallID: "c1", Session: "sess-Q", Record: (&eventLog{}).record}

	ctx1, cancel1 := context.WithCancel(WithLaunch(context.Background(), l))
	ctx2, cancel2 := context.WithCancel(WithLaunch(context.Background(), l))
	addr := proxyOf(t, s.Command(ctx1, ws, "true"))
	if a2 := proxyOf(t, s.Command(ctx2, ws, "true")); a2 != addr {
		t.Fatalf("two commands of one session got proxies %s and %s", addr, a2)
	}
	first := s.sessionProxy("sess-Q")
	cancel1()
	time.Sleep(400 * time.Millisecond)
	if s.sessionProxy("sess-Q") != first || !listening(addr) {
		t.Fatal("the proxy closed with a command still in flight")
	}
	cancel2()
	deadline := time.Now().Add(5 * time.Second)
	for s.sessionProxy("sess-Q") != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.sessionProxy("sess-Q") != nil {
		t.Fatal("a quiet session kept its proxy")
	}
	if listening(addr) {
		t.Fatal("the quiet session's proxy still listens")
	}
	if first.dir != "" {
		if _, err := os.Stat(first.dir); !os.IsNotExist(err) {
			t.Fatalf("socket folder left: %v", err)
		}
	}
	ctx3, cancel3 := context.WithCancel(WithLaunch(context.Background(), l))
	defer cancel3()
	if a3 := proxyOf(t, s.Command(ctx3, ws, "true")); !listening(a3) {
		t.Fatal("the next command's proxy does not listen")
	}
	if next := s.sessionProxy("sess-Q"); next == first || next.proxy == first.proxy {
		t.Fatal("the next command reused the closed proxy")
	}
}

// Commands built and ended at once across sessions, with a quiet time
// shorter than a command, never get a proxy that is closed while they run.
func TestEgressQuietCloseRace(t *testing.T) {
	s, ws := egressProcess(t)
	s.egress.quiet = time.Millisecond
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := Launch{CallID: fmt.Sprint("c", g), Session: fmt.Sprint("sess-", g%3), Record: (&eventLog{}).record}
			for range 25 {
				ctx, cancel := context.WithCancel(WithLaunch(context.Background(), l))
				cmd := s.Command(ctx, ws, "true")
				if cmd.Err != nil {
					errs <- cmd.Err.Error()
					cancel()
					return
				}
				var addr string
				for _, kv := range cmd.Env {
					if v, ok := strings.CutPrefix(kv, "HTTP_PROXY="); ok {
						u, _ := url.Parse(v)
						addr = u.Host
					}
				}
				time.Sleep(3 * time.Millisecond)
				if !listening(addr) {
					errs <- "a command's proxy closed while it was in flight"
				}
				cancel()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

// Close stops every session's proxy, on every platform: none still
// listens, on 127.0.0.1 or, where held, [::1].
func TestEgressCloseStopsEveryProxy(t *testing.T) {
	s, ws := egressProcess(t)
	var addrs []netip.AddrPort
	var v6 []bool
	for _, id := range []string{"sess-1", "sess-2", "sess-3"} {
		ctx, cancel := context.WithCancel(WithLaunch(context.Background(), Launch{CallID: "c", Session: id, Record: (&eventLog{}).record}))
		defer cancel()
		_ = proxyOf(t, s.Command(ctx, ws, "true"))
		ap := s.sessionProxy(id).proxy.Addr()
		addrs = append(addrs, ap)
		v6 = append(v6, listening(net.JoinHostPort("::1", fmt.Sprint(ap.Port()))))
	}
	if addrs[0] == addrs[1] || addrs[1] == addrs[2] {
		t.Fatalf("sessions share a proxy: %v", addrs)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for i, ap := range addrs {
		if listening(ap.String()) {
			t.Errorf("proxy %d still listens on %s after Close", i, ap)
		}
		if v6[i] && listening(net.JoinHostPort("::1", fmt.Sprint(ap.Port()))) {
			t.Errorf("proxy %d still listens on [::1]:%d after Close", i, ap.Port())
		}
	}
}

// With the resolver the relay gets its socket, ids and capability, and the
// generated resolv.conf is bound over the sandbox's; without it, none of that.
func TestEgressRelayArgsBindTheResolver(t *testing.T) {
	p, err := egress.Start(egress.Options{Policy: egressPolicy(t, egress.Config{})})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	e := &egressState{proxy: p, exe: "/abhed", dir: "/d", dns: true}
	binds, wrapped := e.relayArgs([]string{"/bin/bash", "-c", "x"})
	b := strings.Join(binds, " ")
	for _, want := range []string{"--ro-bind /d/resolv.conf /etc/resolv.conf", "--ro-bind /d/dns.sock " + relayDNS, "--cap-add CAP_NET_BIND_SERVICE"} {
		if !strings.Contains(b, want) {
			t.Errorf("binds %q lack %q", b, want)
		}
	}
	w := strings.Join(wrapped, " ")
	if want := fmt.Sprintf("%s %s %d %d -- /bin/bash -c x", egress.DNSArg, relayDNS, os.Getuid(), os.Getgid()); !strings.HasSuffix(w, want) {
		t.Errorf("wrapped %q, want it to end %q", w, want)
	}
	e.dns = false
	binds, wrapped = e.relayArgs([]string{"/bin/true"})
	if b := strings.Join(binds, " "); strings.Contains(b, "resolv.conf") || strings.Contains(b, "--cap-add") {
		t.Errorf("binds without the resolver: %q", b)
	}
	if w := strings.Join(wrapped, " "); strings.Contains(w, egress.DNSArg) {
		t.Errorf("wrapped without the resolver: %q", w)
	}
}

// A process a command leaves running loses its way out when the command's
// call ends: its traffic is refused, recorded under the ended call. On Linux
// the sandbox's PID namespace ends it first, so nothing is recorded at all.
func TestEgressOutlivingProcessIsRefused(t *testing.T) {
	s, ws := egressProcess(t)
	var rec eventLog
	l := Launch{CallID: "call-left", Session: "sess-L", Record: rec.record}
	_ = runLaunched(t, s, ws, l, `(sleep 1; curl -sS -m 10 -o /dev/null http://denied.test/late) >/dev/null 2>&1 &`)
	time.Sleep(3 * time.Second)
	if got := rec.find(func(e map[string]any) bool { return e["path"] == "/late" }); len(got) > 0 {
		t.Fatalf("a process outliving its call was decided on: %v", got)
	}
	auth := rec.find(func(e map[string]any) bool { return e["kind"] == "auth" })
	if runtime.GOOS == "darwin" && len(auth) == 0 {
		t.Fatal("the left-over process's request was not recorded")
	}
	for _, e := range auth {
		if e["call_id"] != "call-left" {
			t.Fatalf("recorded as %v", e)
		}
	}
}
