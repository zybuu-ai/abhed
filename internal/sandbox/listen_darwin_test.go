package sandbox

import (
	"bufio"
	"context"
	"net"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/loopport"
)

// freshListeners makes the next read of the Mac's listeners a new one.
func freshListeners() {
	listeners.mu.Lock()
	listeners.at = time.Time{}
	listeners.mu.Unlock()
}

// listenSandbox is a process sandbox whose commands may listen on ports.
func listenSandbox(t *testing.T, ports ...int) *Process {
	t.Helper()
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec")
	}
	if !canTellSandboxed {
		t.Skip("this build cannot tell a sandboxed process (no cgo)")
	}
	p := DefaultPolicy(t.TempDir())
	p.ListenLoopback = true
	p.ListenPorts = ports
	sb := NewProcess(p)
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

// freePort is a loopback port nothing listens on, outside the reserved range.
func freePort(t *testing.T) int {
	t.Helper()
	for range 20 {
		ports, err := FreeLoopbackPorts(1)
		if err != nil {
			t.Fatal(err)
		}
		if p := int(ports[0]); !loopport.In(p) {
			return p
		}
	}
	t.Fatal("no free port")
	return 0
}

// bindsWith runs a Python bind in sb and reports whether it bound.
func bindsWith(sb *Process, kind, addr string, port int) bool {
	sock := "socket.SOCK_STREAM"
	if kind == "udp" {
		sock = "socket.SOCK_DGRAM"
	}
	line := `python3 -I -c 'import socket,sys; f=socket.AF_INET6 if ":" in sys.argv[1] else socket.AF_INET; s=socket.socket(f, ` + sock + `); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind((sys.argv[1], int(sys.argv[2]))); print("bo" + "und-it")' ` + addr + " " + strconv.Itoa(port)
	o, _ := sb.Command(context.Background(), "/", line).CombinedOutput()
	return strings.Contains(string(o), "bound-it")
}

// With listening on a command serves TCP on a listen port and the Mac
// reaches it, as a browser reaches a dev server. It still connects to
// nothing on loopback, binds no UDP, no port outside the listen ports, no
// port the Mac listens on and none of Abhed's reserved ones.
func TestDevServerListensOnLoopback(t *testing.T) {
	port := freePort(t)
	other := freePort(t)
	serve := "echo served | nc -l 127.0.0.1 " + strconv.Itoa(port)

	off := NewProcess(DefaultPolicy(t.TempDir()))
	off.policy.ListenPorts = []int{port}
	if out, err := off.Command(context.Background(), t.TempDir(), serve).CombinedOutput(); err == nil {
		t.Fatalf("listened without the option: %s", out)
	}
	if canTellSandboxed {
		// The person's agreement turns it on for the commands started after.
		freshListeners()
		want := `(allow network-bind (local tcp "localhost:` + strconv.Itoa(port) + `"))`
		if !SetListen(off, true) || !Listens(off) || !strings.Contains(off.seatbeltProfileWith(0, loopback{}), want) {
			t.Fatal("SetListen did not open listening")
		}
		// An agreement taken back in another process is looked up again.
		still := false
		SetListenWatch(off, func() bool { return still }, nil)
		if Listens(off) || strings.Contains(off.seatbeltProfileWith(0, loopback{}), "(allow network-bind") {
			t.Fatal("listening outlived the agreement")
		}
		if SetListen(off, false); Listens(off) {
			t.Fatal("SetListen did not close listening")
		}
	}

	sb := listenSandbox(t, port, other)
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
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second); err == nil {
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

	freshListeners()
	if bindsWith(sb, "udp", "127.0.0.1", other) {
		t.Fatal("a UDP bind on a listen port was let through")
	}
	if !bindsWith(sb, "tcp", "127.0.0.1", other) {
		t.Fatal("a TCP bind on a listen port was refused")
	}
	if bindsWith(sb, "tcp", "127.0.0.1", freePort(t)) {
		t.Fatal("a port outside the listen ports was bound")
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

	// A listen port the Mac listens on with a wildcard address is not taken.
	wild, err := net.Listen("tcp", ":"+strconv.Itoa(other))
	if err != nil {
		t.Fatal(err)
	}
	defer wild.Close()
	freshListeners()
	if bindsWith(sb, "tcp", "127.0.0.1", other) {
		t.Fatal("bound a listen port the Mac listens on")
	}
	// Nor a reserved one named among the listen ports, on any address.
	lo, hi := loopport.Range()
	reserved := listenSandbox(t, lo, hi)
	for _, p := range []int{lo, hi} {
		for _, addr := range []string{"127.0.0.1", "0.0.0.0", "::1"} {
			if bindsWith(reserved, "tcp", addr, p) {
				t.Fatalf("bound reserved port %s:%d", addr, p)
			}
		}
	}
	if prof := sb.seatbeltProfileWith(0, loopback{}); strings.Contains(prof, `localhost:*`) || strings.Contains(prof, "(local ip ") {
		t.Fatalf("profile opens more than TCP on the listen ports:\n%s", prof)
	}
}

// A program of the person's that starts listening on a port a sandboxed
// command holds (a wildcard bind with SO_REUSEADDR, Go's default, succeeds
// beside it) ends the command's listener, and the sandbox is told.
func TestHostListenerEndsASandboxedOne(t *testing.T) {
	port := freePort(t)
	sb := listenSandbox(t, port)
	var ended atomic.Int64
	SetListenWatch(sb, nil, func(p, pid int) {
		if p == port {
			ended.Store(int64(pid))
		}
	})
	freshListeners()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := sb.Command(ctx, "/", `python3 -I -c 'import socket,sys,time; s=socket.socket(); s.bind(("127.0.0.1", int(sys.argv[1]))); s.listen(); print("listening", flush=True); time.sleep(25)' `+strconv.Itoa(port))
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); !strings.Contains(line, "listening") {
		t.Fatalf("the command did not listen: %q", line)
	}
	host, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("the host could not listen beside it: %v", err)
	}
	defer host.Close()
	freshListeners()
	sweepListeners()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sandboxed listener was not ended")
	}
	if ended.Load() == 0 {
		t.Fatal("the sandbox was not told")
	}
	if HostHolds(port) != true {
		t.Fatal("the host's listener is not seen as the host's")
	}
}

func TestTakenOver(t *testing.T) {
	all := []listener{{port: 3000, pid: 10}, {port: 3000, pid: 20}, {port: 3001, pid: 10}, {port: 3002, pid: 30}, {port: 3002, pid: 31}, {port: 5432, pid: 40}, {port: 5432, pid: 10}}
	inside := func(pid int) bool { return pid == 10 || pid == 30 || pid == 31 }
	got := takenOver(all, map[int]bool{3000: true, 3001: true, 3002: true}, inside)
	// 3000: host 20 beside sandboxed 10; 3002: both sandboxed; 5432 is not a listen port.
	if len(got) != 1 || got[0] != (listener{port: 3000, pid: 10}) {
		t.Fatalf("%v", got)
	}
}

func TestParseListeners(t *testing.T) {
	out := "p947\nf10\nn*:5432\nf11\nn127.0.0.1:8080\nn[::1]:8080\np1\nf3\nnlocalhost:x\n"
	want := []listener{{port: 5432, pid: 947}, {port: 8080, pid: 947}}
	if got := parseListeners(out); !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

// A debug session's loopback rules and listening compose: a Go debug
// command with listening on keeps its ephemeral allows and gains only the
// bind carve-outs; a Python one with listening off keeps its named ports
// alone; with listening on it gains the TCP listen ports, its own kept.
func TestDebugLoopbackAndListenCompose(t *testing.T) {
	count := func(prof, rule string) int { return strings.Count(prof, rule+"\n") }
	bindAll := `(allow network-bind (local ip "localhost:*"))`
	outAll := `(allow network-outbound (remote ip "localhost:*"))`
	lo, _ := loopport.Range()
	reserved := `(deny network-bind (local ip "localhost:` + strconv.Itoa(lo) + `"))`
	devPort := freePort(t)

	listening := listenSandbox(t, devPort)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyDeny := `(deny network-bind (local ip "localhost:` + strconv.Itoa(busy.Addr().(*net.TCPAddr).Port) + `"))`
	freshListeners()
	goDebug := listening.seatbeltProfileWith(0, loopback{on: true, ephemeral: true})
	eLo, _ := loopport.Ephemeral()
	_, hi := loopport.Range()
	if count(goDebug, bindAll) != 1 || count(goDebug, outAll) != 0 || count(goDebug, reserved) != 1 ||
		count(goDebug, `(deny network-bind (local ip "localhost:`+strconv.Itoa(hi)+`"))`) != 1 || count(goDebug, busyDeny) != 1 ||
		strings.Contains(goDebug, "(local tcp ") ||
		!strings.Contains(goDebug, `(allow network-outbound (remote ip "localhost:`+strconv.Itoa(eLo)+`"))`) ||
		strings.Contains(goDebug, `(allow network-outbound (remote ip "localhost:`+strconv.Itoa(lo)+`"))`) {
		t.Fatalf("Go debug with listening on has other rules than intended:\n%s", goDebug)
	}
	// With listening off, the Go debug session's own range denies alone.
	goQuiet := NewProcess(DefaultPolicy(t.TempDir())).seatbeltProfileWith(0, loopback{on: true, ephemeral: true})
	if count(goQuiet, reserved) != 1 || count(goQuiet, busyDeny) != 0 || strings.Contains(goQuiet, "Listening on loopback") {
		t.Fatal("Go debug with listening off has other rules than intended")
	}

	quiet := NewProcess(DefaultPolicy(t.TempDir()))
	py := quiet.seatbeltProfileWith(0, loopback{on: true, ports: []uint16{50101, 50102}})
	if count(py, bindAll) != 0 || count(py, outAll) != 0 || strings.Contains(py, "Listening on loopback") ||
		count(py, `(allow network-bind (local ip "localhost:50101"))`) != 1 || strings.Contains(py, "(deny network-bind") {
		t.Fatalf("Python debug with listening off has other rules than intended:\n%s", py)
	}

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := uint16(held.Addr().(*net.TCPAddr).Port) // #nosec G115 -- a TCP port
	freshListeners()
	pyOn := listening.seatbeltProfileWith(0, loopback{on: true, ports: []uint16{port}})
	if strings.Contains(pyOn, "(deny network-bind") || count(pyOn, bindAll) != 0 ||
		count(pyOn, `(allow network-bind (local ip "localhost:`+strconv.Itoa(int(port))+`"))`) != 1 ||
		count(pyOn, `(allow network-bind (local tcp "localhost:`+strconv.Itoa(devPort)+`"))`) != 1 {
		t.Fatalf("Python debug with listening on has other rules than intended:\n%s", pyOn)
	}
}
