//go:build unix

package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// On a real engine an allowlisted command has no network but the proxy,
// which refuses unlisted hosts; needs ABHED_TEST_RELAY_BINARY (Linux).
func TestContainerAllowlistOnARealEngine(t *testing.T) {
	relay := os.Getenv("ABHED_TEST_RELAY_BINARY")
	if relay == "" {
		t.Skip("set ABHED_TEST_RELAY_BINARY to a Linux build of Abhed to run the allowlist on a real engine")
	}
	old := egressPrefix
	egressPrefix = "abhed128-egr-"
	t.Cleanup(func() { egressPrefix = old })
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "allowed.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	c := NewContainer(Policy{Workspace: ws, Egress: pol, RelayBinary: relay, MinTier: TierContainer})
	if ok, why := c.Available(); !ok {
		t.Skipf("no container engine: %s", why)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(WithLaunch(context.Background(), Launch{CallID: "call-1"}), 3*time.Minute)
	defer cancel()
	script := `port=${HTTP_PROXY##*:}; exec 3<>/dev/tcp/127.0.0.1/$port && printf 'CONNECT denied.test:443 HTTP/1.1\r\nHost: denied.test:443\r\n\r\n' >&3 && head -1 <&3; ` +
		`(exec 4<>/dev/tcp/1.1.1.1/80) 2>/dev/null && echo DIRECT-OPEN || echo DIRECT-REFUSED`
	out, err := c.Command(ctx, ws, "bash -c '"+strings.ReplaceAll(script, "'", `'\''`)+"'").CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if !strings.HasPrefix(string(out), "HTTP/1.1 4") || !strings.Contains(string(out), "DIRECT-REFUSED") {
		t.Fatalf("output %s", out)
	}
}

// On a real engine, with inspect_tls, a command reads the session CA bundle
// where its clients are pointed; needs ABHED_TEST_RELAY_BINARY (Linux).
func TestContainerInspectionCAOnARealEngine(t *testing.T) {
	relay := os.Getenv("ABHED_TEST_RELAY_BINARY")
	if relay == "" {
		t.Skip("set ABHED_TEST_RELAY_BINARY to a Linux build of Abhed to run the allowlist on a real engine")
	}
	old := egressPrefix
	egressPrefix = "abhed128-egr-"
	t.Cleanup(func() { egressPrefix = old })
	pol, err := egress.Compile(egress.Config{InspectTLS: true, Rules: []egress.Rule{{Host: "allowed.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	c := NewContainer(Policy{Workspace: ws, Egress: pol, RelayBinary: relay, MinTier: TierContainer})
	if ok, why := c.Available(); !ok {
		t.Skipf("no container engine: %s", why)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(WithLaunch(context.Background(), Launch{CallID: "call-ca"}), 3*time.Minute)
	defer cancel()
	out, err := c.Command(ctx, ws, `cat "$SSL_CERT_FILE"`).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if ca := strings.TrimSpace(string(c.egProxy.CA())); !strings.Contains(string(out), ca) {
		t.Fatalf("the command did not read the session CA: %.300s", out)
	}
}

// On a real engine the relay runs as a user of its own, so a command cannot
// write frames to its output; needs ABHED_TEST_RELAY_BINARY (Linux).
func TestContainerRelayIsOutOfTheCommandsReach(t *testing.T) {
	relay := os.Getenv("ABHED_TEST_RELAY_BINARY")
	if relay == "" {
		t.Skip("set ABHED_TEST_RELAY_BINARY to a Linux build of Abhed to run the allowlist on a real engine")
	}
	old := egressPrefix
	egressPrefix = "abhed128-egr-"
	t.Cleanup(func() { egressPrefix = old })
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "allowed.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	c := NewContainer(Policy{Workspace: ws, Egress: pol, RelayBinary: relay, MinTier: TierContainer})
	if ok, why := c.Available(); !ok {
		t.Skipf("no container engine: %s", why)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(WithLaunch(context.Background(), Launch{CallID: "call-r"}), 3*time.Minute)
	defer cancel()
	// The relay is the process whose program is the bound relay.
	script := `for p in /proc/[0-9]*; do if [ "$(head -c 16 $p/cmdline 2>/dev/null | tr -d '\0')" = ` + relayInside + ` ]; then ` +
		`echo "relay=$(stat -c %u $p)"; (printf 'x' > $p/fd/1) 2>/dev/null && echo WROTE || echo REFUSED; fi; done; echo "me=$(id -u)"`
	out, err := c.Command(ctx, ws, script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if !strings.Contains(string(out), "relay=") || !strings.Contains(string(out), "REFUSED") || strings.Contains(string(out), "WROTE") {
		t.Fatalf("the relay is within the command's reach: %s", out)
	}
}
