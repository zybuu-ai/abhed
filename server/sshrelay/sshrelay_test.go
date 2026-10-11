package sshrelay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/zybuu-ai/abhed/driver"
)

type fakeHandler struct {
	mu      sync.Mutex
	resizes []driver.WinSize
	pty     *PTY
	ports   []int
}

func (f *fakeHandler) Shell(ctx context.Context, s Session) (int, error) {
	f.mu.Lock()
	f.pty = s.PTY
	f.mu.Unlock()
	buf := make([]byte, 1024)
	for {
		select {
		case ws := <-s.Resize:
			f.mu.Lock()
			f.resizes = append(f.resizes, ws)
			f.mu.Unlock()
		default:
		}
		n, err := s.Stdin.Read(buf)
		if n > 0 {
			if bytes.Contains(buf[:n], []byte("exit")) {
				_, _ = s.Stdout.Write([]byte("bye\r\n"))
				return 7, nil
			}
			_, _ = s.Stdout.Write(buf[:n])
		}
		if err != nil {
			return 0, nil //nolint:nilerr // stdin closing ends the shell normally
		}
	}
}

func (f *fakeHandler) Exec(_ context.Context, command string, s Session) (int, error) {
	if command == "fail" {
		return 0, errors.New("refused by policy")
	}
	in, _ := io.ReadAll(s.Stdin)
	_, _ = io.WriteString(s.Stdout, "ran "+command+" with "+string(in))
	_, _ = io.WriteString(s.Stderr, "err")
	return 3, nil
}

func (f *fakeHandler) Forward(_ context.Context, port int) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	f.ports = append(f.ports, port)
	f.mu.Unlock()
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		_, _ = io.Copy(b, b)
	}()
	return a, nil
}

// tcpPair is two ends of a loopback connection; a net.Pipe has no buffer,
// and both sides of an SSH handshake write first.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		got <- c
	}()
	b, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	a := <-got
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

func dial(t *testing.T, h Handler) (*ssh.Client, ssh.PublicKey, chan Stats) {
	t.Helper()
	key, err := LoadOrCreateHostKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := tcpPair(t)
	stats := make(chan Stats, 1)
	go func() {
		st, _ := Serve(context.Background(), a, Config{HostKey: key}, h)
		stats <- st
	}()
	cc, chans, reqs, err := ssh.NewClientConn(b, "sandbox", &ssh.ClientConfig{User: "anyone",
		HostKeyCallback: ssh.FixedHostKey(key.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c := ssh.NewClient(cc, chans, reqs)
	t.Cleanup(func() { _ = c.Close() })
	return c, key.PublicKey(), stats
}

func TestExecShellAndRefusals(t *testing.T) {
	h := &fakeHandler{}
	c, _, stats := dial(t, h)

	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	s.Stdout, s.Stderr, s.Stdin = &out, &errOut, strings.NewReader("input")
	err = s.Run("echo hi")
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 3 || out.String() != "ran echo hi with input" || errOut.String() != "err" {
		t.Fatalf("exec: %v %q %q", err, out.String(), errOut.String())
	}

	// A refusal says why on stderr and exits non-zero.
	s, _ = c.NewSession()
	errOut.Reset()
	s.Stderr = &errOut
	if err := s.Run("fail"); !errors.As(err, &exit) || exit.ExitStatus() != 255 || !strings.Contains(errOut.String(), "refused by policy") {
		t.Fatalf("refused exec: %v %q", err, errOut.String())
	}

	// A shell on a terminal, resized, ending with its status.
	s, _ = c.NewSession()
	if err := s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	in, _ := s.StdinPipe()
	outp, _ := s.StdoutPipe()
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	if err := s.WindowChange(50, 120); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	_, _ = in.Write([]byte("ls\r"))
	buf := make([]byte, 3)
	if _, err := io.ReadFull(outp, buf); err != nil || string(buf) != "ls\r" {
		t.Fatalf("shell echo %q %v", buf, err)
	}
	_, _ = in.Write([]byte("exit\r"))
	if err := s.Wait(); !errors.As(err, &exit) || exit.ExitStatus() != 7 {
		t.Fatalf("shell end: %v", err)
	}
	h.mu.Lock()
	if h.pty == nil || h.pty.Term != "xterm" || h.pty.Size != (driver.WinSize{Rows: 24, Cols: 80}) {
		t.Fatalf("pty %+v", h.pty)
	}
	if len(h.resizes) != 1 || h.resizes[0] != (driver.WinSize{Rows: 50, Cols: 120}) {
		t.Fatalf("resizes %+v", h.resizes)
	}
	h.mu.Unlock()

	// SFTP is refused, with a pointer to :cp.
	s, _ = c.NewSession()
	errOut.Reset()
	s.Stderr = &errOut
	if err := s.RequestSubsystem("sftp"); err == nil {
		t.Fatal("sftp was served")
	}
	_ = s.Close()

	// Environment is not taken.
	s, _ = c.NewSession()
	if err := s.Setenv("LD_PRELOAD", "/x"); err == nil {
		t.Fatal("an environment variable was accepted")
	}
	_ = s.Close()

	// Forwarding reaches only the sandbox's loopback.
	conn, err := c.Dial("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("ping"))
	if _, err := io.ReadFull(conn, buf[:3]); err != nil || string(buf[:3]) != "pin" {
		t.Fatalf("forward echo %q %v", buf, err)
	}
	_ = conn.Close()
	if _, err := c.Dial("tcp", "10.0.0.1:80"); err == nil {
		t.Fatal("a forward reached past the sandbox's loopback")
	}
	// Remote forwarding is refused.
	if _, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Fatal("remote forwarding was accepted")
	}
	_ = c.Close()
	select {
	case st := <-stats:
		if st.Sessions < 4 || st.Forwards != 1 || st.Refused != 1 || st.User != "anyone" {
			t.Fatalf("stats %+v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not end with the connection")
	}
	if len(h.ports) != 1 || h.ports[0] != 8080 {
		t.Fatalf("ports %v", h.ports)
	}
}

func TestHostKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "ssh_host_ed25519_key")
	k1, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v %v", fi.Mode(), err)
	}
	k2, err := LoadOrCreateHostKey(path)
	if err != nil || !bytes.Equal(k1.PublicKey().Marshal(), k2.PublicKey().Marshal()) {
		t.Fatalf("the key was not kept: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateHostKey(path); err == nil || !strings.Contains(err.Error(), "read by others") {
		t.Fatalf("a readable key: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateHostKey(link); err == nil {
		t.Fatal("a key through a link was read")
	}
}

// A configured key is read, never made: a missing one is an error and
// nothing appears at its path.
func TestLoadHostKeyNeverMakesOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k")
	if _, err := LoadHostKey(path); err == nil {
		t.Fatal("a missing key was loaded")
	}
	if _, err := os.Lstat(path); err == nil {
		t.Fatal("LoadHostKey made a key")
	}
	k, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	again, err := LoadHostKey(path)
	if err != nil || !bytes.Equal(k.PublicKey().Marshal(), again.PublicKey().Marshal()) {
		t.Fatalf("a 0400 key: %v", err)
	}
}

// An end during the handshake is returned as the end, not as a failed
// handshake.
func TestEndDuringHandshake(t *testing.T) {
	key, err := LoadOrCreateHostKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Serve(ctx, a, Config{HostKey: key}, &fakeHandler{})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "handshake") {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not end with its context")
	}
}
