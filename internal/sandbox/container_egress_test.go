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
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/stdiorelay"
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
log=%q
echo "$*" >> "$log"
env | grep 'https\{0,1\}://[^@ ]*@127\.0\.0\.1' | sed 's/^/engine env: /' >> "$log"
case "$1" in
exec)
  shift; envs=""; envfile=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -i) shift ;;
      -w|--user) shift 2 ;;
      -e) case "$2" in *=*) envs="$envs $2" ;; esac; shift 2 ;;
      --env-file) envfile=$2; shift 2 ;;
      *) break ;;
    esac
  done
  shift  # the container's name
  if [ "$1" = %q ]; then shift; exec %q "$@"; fi
  if [ -n "$envfile" ]; then
    ls -l "$envfile" | sed 's/^/env file: /' >> "$log"
    while IFS= read -r l; do export "$l"; done < "$envfile"
  fi
  exec env $envs "$@" ;;
run)
  # Attached, as a container runs: it ends when its input closes.
  cat >/dev/null; exit 0 ;;
inspect)
  echo true; exit 0 ;;
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
	for _, want := range []string{"run --rm -i", "--label " + egressOwnerLabel + "=", "-v /opt/abhed-linux:" + relayInside + ":ro", "--network none",
		"--env-file ", "exec -i --user " + stdiorelay.User(os.Getuid()) + " " + egressPrefix} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in the engine's calls:\n%s", want, calls)
		}
	}
	// The token is in a private file, never in an argument others can list
	// nor in the engine CLI's own environment.
	if strings.Contains(calls, "http://") || strings.Contains(calls, "engine env:") {
		t.Errorf("the call's proxy URL reached the engine's command line or environment:\n%s", calls)
	}
	if !strings.Contains(calls, "env file: -rw-------") {
		t.Errorf("the call's settings were not in a file only this user reads:\n%s", calls)
	}
	if _, after, ok := strings.Cut(calls, "--env-file "); ok {
		f, _, _ := strings.Cut(after, " ")
		deadline := time.Now().Add(5 * time.Second)
		for _, err := os.Stat(f); err == nil && time.Now().Before(deadline); _, err = os.Stat(f) {
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := os.Stat(f); err == nil {
			t.Errorf("the env file outlived the command: %s", f)
		}
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
	host := filepath.Join(c.egDir, "ca", "ca.pem")
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "-v "+filepath.Dir(host)+":"+caDirInside+":ro") {
		t.Fatalf("the bundle was not bound into the container:\n%s", b)
	}
	bundle, err := os.ReadFile(host)
	if err != nil || !strings.Contains(string(bundle), string(c.egProxy.CA())) {
		t.Fatalf("the bundle does not hold the session CA: %v", err)
	}
}

// Under the allowlist the container has no network even where the policy
// also says allow_network, as an SDK caller may set both.
func TestContainerAllowlistHasNoNetworkWhateverAllowNetworkSays(t *testing.T) {
	pol, _ := egress.Compile(egress.Config{})
	c := &Container{runtime: "docker", policy: Policy{Workspace: t.TempDir(), AllowNetwork: true, Egress: pol}}
	args, err := c.runArgs(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(" "+strings.Join(args, " ")+" ", " --network none ") {
		t.Fatalf("no --network none: %v", args)
	}
}

// Closing the sandbox ends every call's container still standing and waits
// for its removal: none is left for a call whose context has not ended, and
// nothing writes to what the sandbox used after Close returns.
func TestContainerCloseRemovesEveryCallsContainer(t *testing.T) {
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	relayPort = func(uint16) uint16 { return uint16(port) } // #nosec G115 -- a port
	t.Cleanup(func() { containerHostOS, relayPort = old, oldPort })
	ctx, cancel := context.WithCancel(WithLaunch(context.Background(), Launch{CallID: "call-close"}))
	defer cancel() // ended only after Close
	if out, err := c.Command(ctx, ws, "echo ran").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "rm -f") {
		t.Fatalf("Close returned with the call's container still standing:\n%s", b)
	}
}

// Close tears the calls' containers down together, not one after another,
// and still waits for every one. Each removal waits until all three have
// started: removals made one at a time never get there, and say so.
func TestContainerCloseTearsCallsDownTogether(t *testing.T) {
	inner, log := fakeEngine(t)
	dir := t.TempDir()
	engine, starts := filepath.Join(dir, "barrier-engine"), filepath.Join(dir, "starts")
	barrier := "#!/bin/sh\nif [ \"$1\" = rm ]; then\n" +
		"  echo start >> '" + starts + "'\n" +
		"  i=0\n" +
		"  while [ \"$(grep -c start '" + starts + "')\" -lt 3 ]; do\n" +
		"    i=$((i+1)); if [ $i -gt 400 ]; then echo alone >> '" + starts + "'; break; fi\n" +
		"    sleep 0.05\n" +
		"  done\n" +
		"fi\nexec " + inner + " \"$@\"\n"
	if err := os.WriteFile(engine, []byte(barrier), 0o755); err != nil { // #nosec G306 -- a test script that must run
		t.Fatal(err)
	}
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "allowed.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	c := &Container{runtime: engine, available: true, policy: Policy{Workspace: ws, Egress: pol, RelayBinary: "/opt/abhed-linux"}}
	c.once.Do(func() {})
	old, oldPort := containerHostOS, relayPort
	containerHostOS = "darwin"
	// The fake engine runs each call's relay on this host: a free port each.
	relayPort = func(uint16) uint16 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		return uint16(ln.Addr().(*net.TCPAddr).Port) // #nosec G115 -- a port
	}
	t.Cleanup(func() { containerHostOS, relayPort = old, oldPort })
	ctx, cancel := context.WithCancel(WithLaunch(context.Background(), Launch{CallID: "call-close"}))
	defer cancel()
	for range 3 {
		if out, err := c.Command(ctx, ws, "echo ran").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	b, _ := os.ReadFile(log)
	if n := strings.Count(string(b), "rm -f"); n < 3 {
		t.Fatalf("Close returned with %d of 3 containers removed:\n%s", n, b)
	}
	// A removal left waiting alone for the others means they ran in turn.
	if sb, _ := os.ReadFile(starts); strings.Contains(string(sb), "alone") || strings.Count(string(sb), "start") < 3 {
		t.Fatalf("the removals did not run together (each waits up to 20 s for the other two):\n%s", sb)
	}
	// A loose ceiling: removals in turn would wait out the barrier, 20 s or more.
	if took > 15*time.Second {
		t.Fatalf("Close took %v", took)
	}
}

// A call still starting its container when the sandbox closes is stopped
// and its container removed before Close returns.
func TestContainerCloseWaitsForACallStillStarting(t *testing.T) {
	inner, log := fakeEngine(t)
	engine := filepath.Join(t.TempDir(), "slow-run-engine")
	slow := "#!/bin/sh\nif [ \"$1\" = run ]; then echo \"$*\" >> " + log + "; sleep 2; fi\nexec " + inner + " \"$@\"\n"
	if err := os.WriteFile(engine, []byte(slow), 0o755); err != nil { // #nosec G306 -- a test script that must run
		t.Fatal(err)
	}
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "allowed.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	c := &Container{runtime: engine, available: true, policy: Policy{Workspace: ws, Egress: pol, RelayBinary: "/opt/abhed-linux"}}
	c.once.Do(func() {})
	old := containerHostOS
	containerHostOS = "darwin"
	t.Cleanup(func() { containerHostOS = old })
	set := make(chan struct{})
	go func() {
		defer close(set)
		_, _ = c.Command(WithLaunch(context.Background(), Launch{CallID: "call-starting"}), ws, "echo ran").CombinedOutput()
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, _ := os.ReadFile(log); strings.Contains(string(b), "run --rm -i") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the container was never started")
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-set:
	default:
		t.Fatal("Close returned while a call was still starting its container")
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "rm -f") {
		t.Fatalf("the starting call's container was not removed:\n%s", b)
	}
}
