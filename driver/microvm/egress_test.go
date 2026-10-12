//go:build !windows

package microvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/microvm/guest"
	"github.com/zybuu-ai/abhed/internal/egress"
)

type events struct {
	mu  sync.Mutex
	evs []egress.Event
}

func (e *events) add(ev egress.Event) { e.mu.Lock(); e.evs = append(e.evs, ev); e.mu.Unlock() }

func (e *events) find(host string) (egress.Event, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.evs {
		if ev.Host == host {
			return ev, true
		}
	}
	return egress.Event{}, false
}

// session is the egress proxy the control plane runs for a session: the
// existing proxy, with its allowlist, serving a Unix socket.
func session(t *testing.T, port int) (*egress.Proxy, string, *events) {
	t.Helper()
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{
		{Host: "allowed.test", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	evs := &events{}
	p, err := egress.Start(egress.Options{Policy: pol, Record: evs.add, Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
		if strings.HasSuffix(host, ".test") {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, fmt.Errorf("no such host %s", host)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	sock := filepath.Join(shortTemp(t), "proxy.sock")
	if err := p.ListenUnix(sock); err != nil {
		t.Fatal(err)
	}
	return p, sock, evs
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return uint16(l.Addr().(*net.TCPAddr).Port)
}

// callEnv is a call's proxy environment as the guest sees it: the guest's
// own loopback port, which in a real VM is the proxy's port number.
func callEnv(t *testing.T, p *egress.Proxy, call string, port uint16) []string {
	t.Helper()
	c, err := p.Issue(call)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.End)
	u, _ := url.Parse(c.URL())
	u.Host = fmt.Sprintf("127.0.0.1:%d", port)
	return []string{"HTTP_PROXY=" + u.String(), "http_proxy=" + u.String(), "HTTPS_PROXY=" + u.String()}
}

func curl(t *testing.T, d *Driver, env []string, target string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := fmt.Sprintf(`curl -s -m 5 -o /dev/null -w '%%{http_code}' %s || true`, target)
	if _, err := d.Exec(context.Background(), id, driver.ExecRequest{Command: cmd, Env: env, Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func origin(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func TestEgressGoesThroughTheSessionProxy(t *testing.T) {
	port := origin(t)
	p, sock, evs := session(t, port)
	d, be := newFake(t)
	gport := freePort(t)
	if _, err := d.Create(context.Background(), driver.Spec{ID: id, Egress: &driver.Egress{Socket: sock, Port: gport}}); err != nil {
		t.Fatal(err)
	}
	if got := be.boots[0].guestPorts; len(got) != 1 || got[0] != guest.EgressPort {
		t.Fatalf("guest ports %v", got)
	}
	env := callEnv(t, p, "call-7", gport)
	if code := curl(t, d, env, fmt.Sprintf("http://allowed.test:%d/", port)); code != "200" {
		t.Fatalf("allowed: %s", code)
	}
	if ev, ok := evs.find("allowed.test"); !ok || ev.CallID != "call-7" || ev.Decision != egress.Allow {
		t.Fatalf("the decision was not recorded under the call: %+v", ev)
	}
	if code := curl(t, d, env, fmt.Sprintf("http://denied.test:%d/", port)); code != "403" {
		t.Fatalf("denied: %s", code)
	}
	// Without the call's credential the proxy refuses.
	if code := curl(t, d, []string{fmt.Sprintf("http_proxy=http://127.0.0.1:%d", gport)}, fmt.Sprintf("http://allowed.test:%d/", port)); code != "407" {
		t.Fatalf("no credential: %s", code)
	}
	// Stopped, the path is gone with the VM.
	if _, err := d.Stop(context.Background(), id, 0); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", gport)); err == nil {
		// The fake guest's forwarder outlives its machine; the host side must not.
		_ = c.SetDeadline(timeoutSoon())
		_, _ = c.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
		if n, _ := c.Read(make([]byte, 1)); n != 0 {
			t.Fatal("the egress path outlived the VM")
		}
		_ = c.Close()
	}
}

func TestNoEgressMeansNoPath(t *testing.T) {
	port := origin(t)
	_, sock, _ := session(t, port)
	d, be := newFake(t)
	if _, err := d.Create(context.Background(), driver.Spec{ID: id}); err != nil {
		t.Fatal(err)
	}
	if len(be.boots[0].guestPorts) != 0 || be.last().guest != nil {
		t.Fatal("a VM without egress was given a port to the host")
	}
	if be.last().srv.Network != nil {
		t.Fatal("the guest was given a forwarder")
	}
	// Even told where the proxy is, a command has no way to it.
	gport := freePort(t)
	env := []string{fmt.Sprintf("http_proxy=http://x:y@127.0.0.1:%d", gport)}
	if code := curl(t, d, env, fmt.Sprintf("http://allowed.test:%d/", port)); code != "000" {
		t.Fatalf("reached something with no egress: %s", code)
	}
	_ = sock
}

func TestEgressSpecIsChecked(t *testing.T) {
	d, _ := newFake(t)
	for _, eg := range []*driver.Egress{{Socket: "rel.sock", Port: 1}, {Socket: "/x.sock"}} {
		if _, err := d.Create(context.Background(), driver.Spec{ID: id, Egress: eg}); !errors.Is(err, driver.ErrInvalid) {
			t.Fatalf("%+v: %v", eg, err)
		}
	}
}

// A driver running for another user connects only to a socket that user serves.
func TestEgressSocketMustBeTheUsers(t *testing.T) {
	_, sock, _ := session(t, 1)
	d, _ := newFake(t)
	d.opts.ImageUser = &Credential{UID: uint32(os.Getuid()) + 1}
	if _, err := d.dialEgress(sock); err == nil || !strings.Contains(err.Error(), "served by uid") {
		t.Fatalf("a socket another user serves was used: %v", err)
	}
	d.opts.ImageUser = &Credential{UID: uint32(os.Getuid())}
	c, err := d.dialEgress(sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	// As root, with no user to check against, no socket is used.
	d.opts.ImageUser = nil
	defer func(f func() int) { euid = f }(euid)
	euid = func() int { return 0 }
	if _, err := d.dialEgress(sock); err == nil {
		t.Fatal("a root driver used a socket without checking who serves it")
	}
}

// The macOS helper's protocol carries the egress port, end to end through
// a helper process.
func TestEgressThroughFakeHelper(t *testing.T) {
	port := origin(t)
	p, sock, _ := session(t, port)
	o := testOptions(t)
	o.Helper = &Helper{Path: fakeVMM(t, "helper")}
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	h := d.be.(*helper)
	h.env = []string{"ABHED_FAKE_VMM=helper"}
	h.probe = func() error { return nil }
	gport := freePort(t)
	if _, err := d.Create(context.Background(), driver.Spec{ID: id, Egress: &driver.Egress{Socket: sock, Port: gport}}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Delete(context.Background(), id) }()
	if code := curl(t, d, callEnv(t, p, "c", gport), fmt.Sprintf("http://allowed.test:%d/", port)); code != "200" {
		t.Fatalf("through the helper: %s", code)
	}
}

func timeoutSoon() time.Time { return time.Now().Add(2 * time.Second) }

// Under inspect_tls the session's CA bundle is placed in the guest before
// the sandbox is ready; a guest that cannot take it refuses the sandbox.
func TestEgressPlacesTheInspectionCA(t *testing.T) {
	port := origin(t)
	_, sock, _ := session(t, port)
	bundle := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	d, be := newFake(t)
	if _, err := d.Create(context.Background(), driver.Spec{ID: id, Egress: &driver.Egress{Socket: sock, Port: freePort(t), CABundle: bundle}}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(be.last().srv.CAFile); err != nil || !bytes.Equal(got, bundle) {
		t.Fatalf("the guest's CA file: %q %v", got, err)
	}
	old, oldBe := newFake(t)
	oldBe.noCA = true
	if _, err := old.Create(context.Background(), driver.Spec{ID: id, Egress: &driver.Egress{Socket: sock, Port: freePort(t), CABundle: bundle}}); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("a guest without the call: %v", err)
	}
}
