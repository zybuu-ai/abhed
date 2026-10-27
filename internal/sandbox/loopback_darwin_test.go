package sandbox

import (
	"context"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// WithLoopback opens loopback ports to one command on the macOS process
// tier, both ways, and nothing beyond; without it loopback stays closed.
func TestLoopbackForOneCommand(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	sb := NewProcess(DefaultPolicy(t.TempDir()))
	run := func(ctx context.Context, line string) (string, error) {
		out, err := sb.Command(ctx, t.TempDir(), line).CombinedOutput()
		return string(out), err
	}
	reach := "nc -z -G 2 127.0.0.1 " + port
	if out, err := run(context.Background(), reach); err == nil {
		t.Fatalf("loopback was open without the option: %s", out)
	}
	ctx := WithLoopback(context.Background())
	if out, err := run(ctx, reach); err != nil {
		t.Fatalf("loopback stayed closed with the option: %v %s", err, out)
	}
	// Listening and connecting inside the command, as an adapter and its program do.
	if out, err := run(ctx, `nc -l 127.0.0.1 47613 > /dev/null & sleep 0.5; echo hi | nc -G 2 127.0.0.1 47613 && echo sent`); err != nil || !strings.Contains(out, "sent") {
		t.Fatalf("a listener inside the command: %v %s", err, out)
	}
	// Another address stays closed (a TEST-NET address, refused before any packet).
	if out, err := run(ctx, "nc -z -G 2 192.0.2.1 80"); err == nil {
		t.Fatalf("an address beyond loopback was reached: %s", out)
	}
	if prof := sb.seatbeltProfileWith(0, true); !strings.Contains(prof, `(allow network-outbound (remote ip "localhost:*"))`) || strings.Contains(prof, `remote ip "*:*"`) {
		t.Fatalf("profile %s", prof)
	}
}

// Under the egress allowlist loopback is never opened: it would reach the
// egress proxies, other calls' and sessions', with allowlists this command
// was never given. A second call's proxy port stays out of reach.
func TestLoopbackRefusedUnderAllowlist(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	p := DefaultPolicy(t.TempDir())
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "allowed.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	p.Egress = pol
	sb := NewProcess(p)
	cmd := sb.Command(WithLoopback(context.Background()), t.TempDir(), "nc -z -G 2 127.0.0.1 "+strconv.Itoa(other.Addr().(*net.TCPAddr).Port))
	if cmd.Err == nil || !strings.Contains(cmd.Err.Error(), "allowlist") {
		out, _ := cmd.CombinedOutput()
		t.Fatalf("loopback opened under the allowlist: %v %s", cmd.Err, out)
	}
}
