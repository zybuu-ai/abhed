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
	if prof := sb.seatbeltProfileWith(0, loopback{on: true}); !strings.Contains(prof, `(allow network-outbound (remote ip "localhost:*"))`) || strings.Contains(prof, `remote ip "*:*"`) {
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

// Loopback to named ports holds under the allowlist: the command listens and
// reaches its own ports, and another port on loopback (such as another
// call's egress proxy) stays out of reach.
func TestLoopbackPortsUnderAllowlist(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	go func() {
		for {
			c, err := other.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "allowed.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(t.TempDir())
	p.Egress = pol
	sb := NewProcess(p)
	ports, err := FreeLoopbackPorts(1)
	if err != nil {
		t.Fatal(err)
	}
	own := strconv.Itoa(int(ports[0]))
	ctx := WithLoopbackPorts(context.Background(), ports...)
	run := func(line string) (string, error) {
		out, err := sb.Command(ctx, t.TempDir(), line).CombinedOutput()
		return string(out), err
	}
	if out, err := run("nc -l 127.0.0.1 " + own + " > /dev/null & sleep 0.5; echo hi | nc -G 2 127.0.0.1 " + own + " && echo sent"); err != nil || !strings.Contains(out, "sent") {
		t.Fatalf("the command's own port: %v %s", err, out)
	}
	if out, err := run("nc -z -G 2 127.0.0.1 " + strconv.Itoa(other.Addr().(*net.TCPAddr).Port)); err == nil {
		t.Fatalf("another loopback port was reached: %s", out)
	}
	prof := sb.seatbeltProfileWith(0, loopback{on: true, ports: ports})
	if strings.Contains(prof, `remote ip "localhost:*"`) || !strings.Contains(prof, `remote ip "localhost:`+own+`"`) {
		t.Fatalf("profile %s", prof)
	}
}
