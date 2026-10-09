//go:build !windows

package microvm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver/microvm/guest"
)

// The test binary doubles as a fake VMM: as the macOS helper or the jailer
// it serves the guest protocol on the vsock socket, running commands on
// this host. It confines nothing; it stands in for the VM so the driver's
// process handling, files and wire are exercised for real.
func TestMain(m *testing.M) {
	switch os.Getenv("ABHED_FAKE_VMM") {
	case "helper":
		os.Exit(fakeHelper(os.Args[1:]))
	case "jailer":
		os.Exit(fakeJailer(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeHelper(args []string) int {
	if len(args) == 1 && args[0] == "check" {
		fmt.Println(`{"protocol":1,"supported":true}`)
		return 0
	}
	if len(args) != 3 || args[0] != "run" || args[1] != "--config" {
		return 2
	}
	b, err := os.ReadFile(args[2])
	if err != nil {
		return 3
	}
	var cfg helperConfig
	if json.Unmarshal(b, &cfg) != nil || cfg.Network != "none" {
		return 4
	}
	// The helper protocol: the VM ends when the driver's end of stdin does.
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()
	ports := map[uint32]string{}
	for _, p := range cfg.GuestPorts {
		ports[p.Port] = p.Socket
	}
	return serveVsock(cfg.VsockSocket, func(p uint32) string { return ports[p] })
}

func fakeJailer(args []string) int {
	flags := map[string]string{}
	for i := 0; i+1 < len(args) && args[i] != "--"; i += 2 {
		flags[args[i]] = args[i+1]
	}
	root := filepath.Join(flags["--chroot-base-dir"], filepath.Base(flags["--exec-file"]), flags["--id"], "root")
	b, err := os.ReadFile(filepath.Join(root, chConfig))
	if err != nil {
		return 3
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return 4
	}
	if _, nic := cfg["network-interfaces"]; nic {
		return 5
	}
	if os.Getenv("ABHED_FAKE_VMM_HANG") != "" {
		time.Sleep(time.Hour)
	}
	return serveVsock(filepath.Join(root, chVsock), func(p uint32) string {
		return filepath.Join(root, fmt.Sprintf("%s_%d", chVsock, p))
	})
}

// serveVsock answers the hybrid vsock handshake, then the guest protocol.
func serveVsock(sock string, guestPort func(uint32) string) int {
	l, err := net.Listen("unix", sock)
	if err != nil {
		return 6
	}
	fmt.Println("fake VMM up")
	dir, _ := os.Getwd()
	s := &guest.Server{Env: []string{"PATH=/usr/bin:/bin", "ABHED_SANDBOX=microvm"}, Dir: dir, Shell: "/bin/sh",
		PowerOff: func() { os.Exit(0) }}
	if _, err := os.Stat(guestPort(guest.EgressPort)); err == nil {
		s.Network = fakeNetwork(guestPort)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return 7
		}
		go func() {
			r := bufio.NewReader(c)
			line, err := r.ReadString('\n')
			port, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "CONNECT ")))
			if err != nil || port != guest.Port {
				_ = c.Close()
				return
			}
			_, _ = fmt.Fprintf(c, "OK %d\n", 1<<30)
			s.ServeConn(bufConn{c, r})
		}()
	}
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// copyImage stands in for buildImage: the "image" is a copy of the folder.
func copyImage(_ context.Context, _, src, dir string, _ int, _ *Credential) (string, error) {
	dst := filepath.Join(dir, "guestws")
	return dst, os.CopyFS(dst, os.DirFS(src))
}

// fakeBackend boots machines in this process.
type fakeBackend struct {
	mu       sync.Mutex
	err      error // returned by check
	silent   bool  // the guest never answers
	delay    time.Duration
	boots    []bootSpec
	machines []*fakeMachine
}

func (f *fakeBackend) name() string { return "fake" }

func (f *fakeBackend) recover(context.Context) ([]string, error) { return nil, nil }
func (f *fakeBackend) check() error                              { f.mu.Lock(); defer f.mu.Unlock(); return f.err }

func (f *fakeBackend) boot(_ context.Context, b bootSpec) (machine, error) {
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	m := &fakeMachine{exited: make(chan struct{}), silent: f.silent, dir: os.TempDir()}
	m.srv = &guest.Server{Env: []string{"PATH=/usr/bin:/bin"}, Dir: m.dir, Shell: "/bin/sh", PowerOff: m.powerOff}
	// The fake image is a copy of the workspace, standing in for the guest's.
	if b.workspace != "" {
		tr, err := guest.NewTracker(b.workspace)
		if err != nil {
			return nil, err
		}
		m.dir, m.srv.Dir, m.srv.Workspace = b.workspace, b.workspace, tr
	}
	if len(b.guestPorts) > 0 {
		dir, err := os.MkdirTemp("/tmp", "mvmg")
		if err != nil {
			return nil, err
		}
		lns, err := listenGuestPorts(b.guestPorts, func(p uint32) string { return filepath.Join(dir, strconv.Itoa(int(p))) }, nil)
		if err != nil {
			return nil, err
		}
		m.guest, m.guestDir = lns, dir
		m.srv.Network = fakeNetwork(func(p uint32) string { return filepath.Join(dir, strconv.Itoa(int(p))) })
	}
	f.boots = append(f.boots, b)
	f.machines = append(f.machines, m)
	return m, nil
}

// fakeNetwork stands in for the guest agent's forwarder: the "guest"
// shares this host's loopback, and reaches the host side's socket directly.
func fakeNetwork(path func(uint32) string) func(uint16) error {
	var mu sync.Mutex
	var started bool
	return func(port uint16) error {
		mu.Lock()
		defer mu.Unlock()
		if started {
			return nil
		}
		_, err := guest.Forward(port, func() (io.ReadWriteCloser, error) { return net.Dial("unix", path(guest.EgressPort)) })
		started = err == nil
		return err
	}
}

func (f *fakeBackend) last() *fakeMachine {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.machines[len(f.machines)-1]
}

type fakeMachine struct {
	srv      *guest.Server
	dir      string
	silent   bool
	guest    map[uint32]net.Listener
	guestDir string

	mu       sync.Mutex
	exited   chan struct{}
	killed   int
	shutdown int
}

func (m *fakeMachine) dial(ctx context.Context) (net.Conn, error) {
	select {
	case <-m.exited:
		return nil, errors.New("connection refused")
	default:
	}
	host, g := net.Pipe()
	if m.silent {
		go func() { <-ctx.Done(); _ = g.Close() }()
		return host, nil
	}
	go m.srv.ServeConn(g)
	return host, nil
}

func (m *fakeMachine) powerOff() {
	m.mu.Lock()
	m.shutdown++
	m.mu.Unlock()
	m.exit()
}

func (m *fakeMachine) exit() {
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.exited:
	default:
		close(m.exited)
	}
}

func (m *fakeMachine) done() <-chan struct{} { return m.exited }
func (m *fakeMachine) console() string       { return "fake console" }

func (m *fakeMachine) guestListener(port uint32) net.Listener { return m.guest[port] }

func (m *fakeMachine) kill() error {
	m.mu.Lock()
	m.killed++
	m.mu.Unlock()
	closeAll(m.guest)
	if m.guestDir != "" {
		_ = os.RemoveAll(m.guestDir)
	}
	m.exit()
	return nil
}

func (m *fakeMachine) counts() (killed, shutdown int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.killed, m.shutdown
}

func testOptions(t *testing.T) Options {
	t.Helper()
	dir := shortTemp(t)
	o := Options{Kernel: filepath.Join(dir, "vmlinux"), Rootfs: filepath.Join(dir, "rootfs.ext4"), StateDir: filepath.Join(dir, "state"), BootTimeoutSeconds: 2}
	for _, p := range []string{o.Kernel, o.Rootfs} {
		if err := os.WriteFile(p, []byte(filepath.Base(p)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	o.defaults()
	return o
}

// shortTemp is a temporary folder with a short path: a Unix socket path is
// limited to about 100 bytes.
func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mvm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
