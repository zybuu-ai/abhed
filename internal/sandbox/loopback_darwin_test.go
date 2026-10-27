package sandbox

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/loopport"
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
	if !errors.Is(cmd.Err, ErrLoopbackUnderEgress) {
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

// The system's ephemeral loopback ports but Abhed's reserved range: under
// the egress proxy the command runs, reaches a port the system handed out,
// and reaches no listener inside the range, on 127.0.0.1 or [::1].
func TestLoopbackEphemeral(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	accept := func(ln net.Listener) {
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
	}
	// A reserved range inside the ephemeral one here, to show it is carved out.
	base := 60100
	t.Setenv(loopport.Env, strconv.Itoa(base)+"-"+strconv.Itoa(base+15))
	lns, err := loopport.ListenBoth(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lns {
		defer l.Close()
		accept(l)
	}
	inside := strconv.Itoa(lns[0].Addr().(*net.TCPAddr).Port)
	outside, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer outside.Close()
	accept(outside)
	if loopport.In(outside.Addr().(*net.TCPAddr).Port) {
		t.Skip("the system's port landed in the test range")
	}
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "allowed.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(t.TempDir())
	p.Egress = pol
	sb := NewProcess(p)
	own := t.TempDir()
	ctx := WithPrivateDirs(WithLoopbackEphemeral(context.Background()), own)
	run := func(line string) (string, error) {
		cmd := sb.Command(ctx, t.TempDir(), line)
		if cmd.Err != nil {
			return "", cmd.Err
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("nc -z -G 2 127.0.0.1 " + strconv.Itoa(outside.Addr().(*net.TCPAddr).Port)); err != nil {
		t.Fatalf("a port the system handed out: %v %s", err, out)
	}
	if out, err := run("nc -z -G 2 127.0.0.1 " + inside); err == nil {
		t.Fatalf("a listener inside the reserved range was reached: %s", out)
	}
	if len(lns) == 2 {
		if out, err := run("nc -6 -z -G 2 ::1 " + inside); err == nil {
			t.Fatalf("an [::1] listener inside the reserved range was reached: %s", out)
		}
	}
	prof := sb.seatbeltProfileWith(0, loopback{on: true, ephemeral: true})
	if strings.Contains(prof, `remote ip "localhost:*"`) || strings.Contains(prof, `remote ip "localhost:`+inside+`"`) ||
		!strings.Contains(prof, `remote ip "localhost:`+strconv.Itoa(outside.Addr().(*net.TCPAddr).Port)+`"`) {
		t.Fatal("the profile does not name the ephemeral ports but the range")
	}
	if _, err := os.Stat(filepath.Join(own, "profile.sb")); err != nil {
		t.Fatalf("the long profile was not put in the command's own folder: %v", err)
	}
	// Without a folder of its own, the long profile has nowhere to go.
	if cmd := sb.Command(WithLoopbackEphemeral(context.Background()), t.TempDir(), "true"); cmd.Err == nil {
		t.Fatal("ran without a folder for its profile")
	}
}
