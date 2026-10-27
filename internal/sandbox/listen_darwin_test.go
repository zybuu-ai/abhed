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

	"github.com/zybuu-ai/abhed/internal/loopport"
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
	lo, hi := loopport.Range()
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

// A debug session's loopback rules and listening compose: a Go debug
// command with listening on keeps its ephemeral allows and gains only the
// bind carve-outs; a Python one with listening off keeps its named ports
// alone; and with listening on, its named ports are never carved out.
func TestDebugLoopbackAndListenCompose(t *testing.T) {
	count := func(prof, rule string) int { return strings.Count(prof, rule+"\n") }
	bindAll := `(allow network-bind (local ip "localhost:*"))`
	outAll := `(allow network-outbound (remote ip "localhost:*"))`
	lo, _ := loopport.Range()
	reserved := `(deny network-bind (local ip "localhost:` + strconv.Itoa(lo) + `"))`

	listening := NewProcess(DefaultPolicy(t.TempDir()))
	SetListen(listening, true)
	listeners.mu.Lock()
	listeners.at = time.Time{}
	listeners.mu.Unlock()
	goDebug := listening.seatbeltProfileWith(0, loopback{on: true, ephemeral: true})
	eLo, _ := loopport.Ephemeral()
	if count(goDebug, bindAll) != 1 || count(goDebug, outAll) != 0 || count(goDebug, reserved) != 1 ||
		!strings.Contains(goDebug, `(allow network-outbound (remote ip "localhost:`+strconv.Itoa(eLo)+`"))`) ||
		strings.Contains(goDebug, `(allow network-outbound (remote ip "localhost:`+strconv.Itoa(lo)+`"))`) {
		t.Fatal("Go debug with listening on has other rules than intended")
	}

	quiet := NewProcess(DefaultPolicy(t.TempDir()))
	py := quiet.seatbeltProfileWith(0, loopback{on: true, ports: []uint16{50101, 50102}})
	if count(py, bindAll) != 0 || count(py, outAll) != 0 || strings.Contains(py, "Listening on loopback") ||
		count(py, `(allow network-bind (local ip "localhost:50101"))`) != 1 || strings.Contains(py, "(deny network-bind") {
		t.Fatalf("Python debug with listening off has other rules than intended:\n%s", py)
	}

	// Its named ports held by its own listeners stay bindable with listening on.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := uint16(held.Addr().(*net.TCPAddr).Port) // #nosec G115 -- a TCP port
	listeners.mu.Lock()
	listeners.at = time.Time{}
	listeners.mu.Unlock()
	pyOn := listening.seatbeltProfileWith(0, loopback{on: true, ports: []uint16{port}})
	if strings.Contains(pyOn, `(deny network-bind (local ip "localhost:`+strconv.Itoa(int(port))+`"))`) || count(pyOn, bindAll) != 1 || count(pyOn, reserved) != 1 {
		t.Fatal("Python debug with listening on carves out its own port, or lacks the listen rules")
	}
}
