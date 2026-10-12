//go:build unix

package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// fakeEngine is an engine CLI that logs each call, runs "exec" on the host as
// the relay (this test binary), and ignores "run" and "rm".
func fakeEngine(t *testing.T) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$1" in
exec)
  shift; envs=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -i) shift ;;
      -w|--user) shift 2 ;;
      -e) case "$2" in *=*) envs="$envs $2" ;; esac; shift 2 ;;
      *) break ;;
    esac
  done
  shift  # the container's name
  if [ "$1" = %q ]; then shift; exec %q "$@"; fi
  exec env $envs "$@" ;;
esac
exit 0
`, log, relayInside, self)
	path = filepath.Join(dir, "engine")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { // #nosec G306 -- a test script that must run
		t.Fatal(err)
	}
	return path, log
}

// Under the allowlist a container tier command reaches the proxy, and only
// it, through the relay its container runs, by its call's own way in: a
// request with no credentials is the call's. The container goes with the call.
func TestContainerCommandReachesTheProxyThroughTheRelay(t *testing.T) {
	engine, log := fakeEngine(t)
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "allowed.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	c := &Container{runtime: engine, available: true, policy: Policy{Workspace: ws, Egress: pol, RelayBinary: "/opt/abhed-linux"}}
	c.once.Do(func() {})
	old, oldPort := containerHostOS, relayPort
	containerHostOS = "darwin"
	// The fake engine runs the relay on this host, beside the proxy's own port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	relayPort = func(uint16) uint16 { return uint16(port) } // #nosec G115 -- a port
	t.Cleanup(func() { containerHostOS, relayPort = old, oldPort; _ = c.Close() })
	var rec eventLog
	ctx, cancel := context.WithCancel(WithLaunch(context.Background(), Launch{CallID: "call-1", Record: rec.record}))
	// The command connects to its proxy on its own loopback, as a client would.
	cmd := c.Command(ctx, ws, `printf 'CONNECT denied.test:443 HTTP/1.1\r\nHost: denied.test:443\r\n\r\n' | nc -w 2 127.0.0.1 `+fmt.Sprint(port)+` | head -1; echo "proxy=$HTTP_PROXY"`)
	if cmd.Err != nil {
		t.Fatal(cmd.Err)
	}
	out, err := cmd.CombinedOutput()
	cancel()
	if err != nil || !strings.Contains(string(out), "proxy=http://") {
		t.Fatalf("%v %s", err, out)
	}
	b, _ := os.ReadFile(log)
	calls := string(b)
	for _, want := range []string{"run --rm -d", "-v /opt/abhed-linux:" + relayInside + ":ro", "--network none", "exec -i " + egressPrefix,
		"-e HTTPS_PROXY "} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in the engine's calls:\n%s", want, calls)
		}
	}
	// The token is passed by name, never in an argument others can list.
	if strings.Contains(calls, "http://") {
		t.Errorf("the call's proxy URL is on the engine's command line:\n%s", calls)
	}
	// The relay answered for the proxy: the denied host was refused by it,
	// as the call's, though the request carried no credentials.
	if line := strings.SplitN(string(out), "\n", 2)[0]; !strings.HasPrefix(line, "HTTP/1.1 403") {
		t.Fatalf("the proxy did not refuse the call's request through the relay: %q\n%s", line, out)
	}
	rec.waitFor(t, "the call's refused CONNECT", func(e map[string]any) bool {
		return e["call_id"] == "call-1" && e["host"] == "denied.test" && e["decision"] == "deny"
	})
}

// Without a relay for an engine in a VM, the tier is refused, never given the network.
func TestContainerAllowlistNeedsARelay(t *testing.T) {
	pol, _ := egress.Compile(egress.Config{})
	c := &Container{policy: Policy{Workspace: t.TempDir(), Egress: pol}}
	old := containerHostOS
	containerHostOS = "darwin"
	t.Cleanup(func() { containerHostOS = old })
	if ok, why := c.Available(); ok || !strings.Contains(why, "relay_binary") {
		t.Fatalf("available %v: %s", ok, why)
	}
}

// With inspect_tls each command's container has the session CA bundle bound
// in read-only, and its clients are pointed at it there.
func TestContainerCommandGetsTheInspectionCA(t *testing.T) {
	engine, log := fakeEngine(t)
	pol, err := egress.Compile(egress.Config{InspectTLS: true, Rules: []egress.Rule{{Host: "allowed.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	c := &Container{runtime: engine, available: true, policy: Policy{Workspace: ws, Egress: pol, RelayBinary: "/opt/abhed-linux"}}
	c.once.Do(func() {})
	old, oldPort := containerHostOS, relayPort
	containerHostOS = "darwin"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	relayPort = func(uint16) uint16 { return uint16(port) } // #nosec G115 -- a port
	t.Cleanup(func() { containerHostOS, relayPort = old, oldPort; _ = c.Close() })
	ctx, cancel := context.WithCancel(WithLaunch(context.Background(), Launch{CallID: "call-ca"}))
	defer cancel()
	out, err := c.Command(ctx, ws, `echo "ca=$SSL_CERT_FILE node=$NODE_EXTRA_CA_CERTS"`).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if want := "ca=" + caInside + " node=" + caInside; !strings.Contains(string(out), want) {
		t.Fatalf("the command was not pointed at the bundle: %s", out)
	}
	host := filepath.Join(c.egDir, "ca.pem")
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "-v "+host+":"+caInside+":ro") {
		t.Fatalf("the bundle was not bound into the container:\n%s", b)
	}
	bundle, err := os.ReadFile(host)
	if err != nil || !strings.Contains(string(bundle), string(c.egProxy.CA())) {
		t.Fatalf("the bundle does not hold the session CA: %v", err)
	}
}
