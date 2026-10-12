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
