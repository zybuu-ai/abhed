package sandbox

import (
	"bufio"
	"context"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// With ListenLoopback a command serves on loopback and the Mac reaches it,
// as a browser reaches a dev server; it still connects to nothing on
// loopback, and binds neither a port the Mac listens on nor a reserved one.
func TestDevServerListensOnLoopback(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	ports, err := FreeLoopbackPorts(1)
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(int(ports[0]))
	serve := "echo served | nc -l 127.0.0.1 " + port
	off := NewProcess(DefaultPolicy(t.TempDir()))
	if out, err := off.Command(context.Background(), t.TempDir(), serve).CombinedOutput(); err == nil {
		t.Fatalf("listened without the option: %s", out)
	}

	// The person's agreement turns it on for the commands started after.
	if !SetListen(off, true) || !Listens(off) || !strings.Contains(off.seatbeltProfileWith(0, loopback{}), `(allow network-bind (local ip "localhost:*"))`) {
		t.Fatal("SetListen did not open listening")
	}
	if SetListen(off, false); Listens(off) || strings.Contains(off.seatbeltProfileWith(0, loopback{}), `(allow network-bind`) {
		t.Fatal("SetListen did not close listening")
	}

	p := DefaultPolicy(t.TempDir())
	p.ListenLoopback = true
	sb := NewProcess(p)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := sb.Command(ctx, t.TempDir(), serve)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var got string
	for i := 0; i < 50 && got == ""; i++ {
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
			got, _ = bufio.NewReader(c).ReadString('\n')
			_ = c.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Wait()
	if strings.TrimSpace(got) != "served" {
		t.Fatalf("the Mac did not reach the command's server: %q %s", got, out.String())
	}

	// No connection out on loopback.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if o, err := sb.Command(context.Background(), t.TempDir(), "nc -z -G 2 127.0.0.1 "+strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)).CombinedOutput(); err == nil {
		t.Fatalf("connected out on loopback: %s", o)
	}

	// A port the Mac listens on with a wildcard address is not taken over.
	wild, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer wild.Close()
	listeners.mu.Lock()
	listeners.at = time.Time{}
	listeners.mu.Unlock()
	busy := strconv.Itoa(wild.Addr().(*net.TCPAddr).Port)
	hijack := `python3 -I -c 'import socket,sys; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("127.0.0.1", int(sys.argv[1]))); print("bo" + "und-it")' ` + busy
	if o, _ := sb.Command(context.Background(), t.TempDir(), hijack).CombinedOutput(); strings.Contains(string(o), "bound-it") {
		t.Fatalf("bound a port the Mac listens on: %s", o)
	}
	lo, hi := reservedLoopback()
	// Each carve-out holds on its own port, for every address of it.
	for _, p := range []int{lo, (lo + hi) / 2, hi} {
		for _, addr := range []string{"127.0.0.1", "0.0.0.0", "::1"} {
			bind := `python3 -I -c 'import socket,sys; f=socket.AF_INET6 if ":" in sys.argv[1] else socket.AF_INET; s=socket.socket(f); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind((sys.argv[1], int(sys.argv[2]))); print("bo" + "und-it")' ` + addr + " " + strconv.Itoa(p)
			if o, _ := sb.Command(context.Background(), t.TempDir(), bind).CombinedOutput(); strings.Contains(string(o), "bound-it") {
				t.Fatalf("bound reserved port %s:%d: %s", addr, p, o)
			}
		}
	}
	prof := sb.seatbeltProfileWith(0, loopback{})
	if !strings.Contains(prof, `(deny network-bind (local ip "localhost:`+strconv.Itoa(lo)+`"))`) ||
		!strings.Contains(prof, `(deny network-bind (local ip "localhost:`+strconv.Itoa(hi)+`"))`) ||
		strings.Contains(prof, `(allow network-outbound (remote ip "localhost:*"))`) {
		t.Fatalf("profile %s", prof)
	}
}

func TestParseListeners(t *testing.T) {
	out := "p947\nf10\nn*:5432\nf11\nn127.0.0.1:8080\nn[::1]:8080\np1\nf3\nnlocalhost:x\n"
	if got := parseListeners(out); len(got) != 2 || got[0] != 5432 || got[1] != 8080 {
		t.Fatalf("%v", got)
	}
}
