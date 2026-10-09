//go:build !windows

package guest

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
	"sync/atomic"
	"testing"
	"time"
)

// pair serves one connection with s and returns the host end.
func pair(t *testing.T, s *Server) net.Conn {
	t.Helper()
	host, g := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); s.ServeConn(g) }()
	t.Cleanup(func() {
		_ = host.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the guest server did not return")
		}
	})
	return host
}

func server(t *testing.T) *Server {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Env: []string{"PATH=/usr/bin:/bin", "HOME=" + dir}, Dir: dir, Shell: "/bin/sh"}
}

func run(t *testing.T, s *Server, req ExecRequest, stdin io.Reader) (int, string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	code, err := Exec(context.Background(), pair(t, s), req, Streams{Stdin: stdin, Stdout: &out, Stderr: &errb})
	return code, out.String(), errb.String(), err
}

func TestExecCommandStreamsAndExitCode(t *testing.T) {
	code, out, errs, err := run(t, server(t), ExecRequest{Command: "echo out; echo err >&2; exit 3"}, nil)
	if err != nil || code != 3 || out != "out\n" || errs != "err\n" {
		t.Fatalf("code=%d out=%q err=%q %v", code, out, errs, err)
	}
}

func TestExecArgvIsNotAShellLine(t *testing.T) {
	code, out, _, err := run(t, server(t), ExecRequest{Argv: []string{"/bin/echo", "a;b", "$HOME"}}, nil)
	if err != nil || code != 0 || out != "a;b $HOME\n" {
		t.Fatalf("code=%d out=%q %v", code, out, err)
	}
}

func TestExecStdinAndLargeOutput(t *testing.T) {
	in := strings.Repeat("x", 3*MaxChunk+17)
	code, out, _, err := run(t, server(t), ExecRequest{Argv: []string{"/bin/cat"}}, strings.NewReader(in))
	if err != nil || code != 0 || out != in {
		t.Fatalf("code=%d len(out)=%d %v", code, len(out), err)
	}
}

func TestExecSignalIs128Plus(t *testing.T) {
	code, _, _, err := run(t, server(t), ExecRequest{Command: "kill -9 $$"}, nil)
	if err != nil || code != 137 {
		t.Fatalf("code=%d %v", code, err)
	}
}

func TestExecCwdRelativeToDir(t *testing.T) {
	s := server(t)
	if err := os.Mkdir(filepath.Join(s.Dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, out, _, err := run(t, s, ExecRequest{Command: "pwd", Cwd: "sub"}, nil)
	if err != nil || strings.TrimSpace(out) != filepath.Join(s.Dir, "sub") {
		t.Fatalf("out=%q %v", out, err)
	}
}

func TestExecEnvAddsButNeverReplaces(t *testing.T) {
	s := server(t)
	_, out, _, err := run(t, s, ExecRequest{Command: "echo $EXTRA", Env: []string{"EXTRA=1"}}, nil)
	if err != nil || out != "1\n" {
		t.Fatalf("out=%q %v", out, err)
	}
	_, _, _, err = run(t, s, ExecRequest{Command: "true", Env: []string{"PATH=/tmp"}}, nil)
	var ge *Error
	if !errors.As(err, &ge) || !strings.Contains(ge.Message, "PATH") {
		t.Fatalf("replacing PATH: %v", err)
	}
	// A bare entry may be a secret's value: the refusal gives its position only.
	_, _, _, err = run(t, s, ExecRequest{Command: "true", Env: []string{"A=1", "ghp_secretvalue"}}, nil)
	if !errors.As(err, &ge) || strings.Contains(ge.Message, "ghp_") || !strings.Contains(ge.Message, "entry 2") {
		t.Fatalf("a bare entry: %v", err)
	}
}

func TestExecMissingCommandIsAnError(t *testing.T) {
	_, _, _, err := run(t, server(t), ExecRequest{Argv: []string{"/no/such/binary"}}, nil)
	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("want a guest error, got %v", err)
	}
}

func TestExecRefusesBadRequestBeforeSending(t *testing.T) {
	for _, req := range []ExecRequest{{}, {Argv: []string{"a"}, Command: "b"}, {TTY: true, Command: "x"}} {
		host, g := net.Pipe()
		_, err := Exec(context.Background(), host, req, Streams{})
		_ = g.Close()
		if err == nil {
			t.Fatalf("%+v was accepted", req)
		}
	}
}

func TestTerminalRunsAShellWithResize(t *testing.T) {
	s := server(t)
	in, inw := io.Pipe()
	resize := make(chan Size, 1)
	var out syncBuffer
	codeCh := make(chan int, 1)
	errCh := make(chan error, 1)
	go func() {
		code, err := Exec(context.Background(), pair(t, s), ExecRequest{TTY: true, Rows: 20, Cols: 80},
			Streams{Stdin: in, Stdout: &out, Resize: resize})
		codeCh <- code
		errCh <- err
	}()
	resize <- Size{Rows: 33, Cols: 101}
	time.Sleep(200 * time.Millisecond)
	_, _ = io.WriteString(inw, "stty size; echo mark-$((40+2)); exit 7\n")
	select {
	case err := <-errCh:
		if code := <-codeCh; err != nil || code != 7 {
			t.Fatalf("code=%d %v", code, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the terminal did not end")
	}
	_ = inw.Close()
	if !strings.Contains(out.String(), "mark-42") || !strings.Contains(out.String(), "33 101") {
		t.Fatalf("terminal output %q", out.String())
	}
}

func TestCancelKillsTheCommand(t *testing.T) {
	s := server(t)
	marker := filepath.Join(s.Dir, "still-running")
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := Exec(ctx, pair(t, s), ExecRequest{Command: "sleep 1; touch " + marker}, Streams{})
		errCh <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command kept running after the host went away")
	}
}

func TestPingAndShutdown(t *testing.T) {
	s := server(t)
	var off atomic.Int32
	powered := make(chan struct{})
	s.PowerOff = func() { off.Add(1); close(powered) }
	if err := Ping(context.Background(), pair(t, s)); err != nil {
		t.Fatal(err)
	}
	if off.Load() != 0 {
		t.Fatal("ping powered off")
	}
	if err := Shutdown(context.Background(), pair(t, s)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-powered:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not power off")
	}
}

// fakeGuest answers the hello, reads the request, then sends lines.
func fakeGuest(t *testing.T, lines ...string) net.Conn {
	t.Helper()
	host, g := net.Pipe()
	go func() {
		defer func() { _ = g.Close() }()
		c := newConn(g)
		if _, err := c.read(); err != nil {
			return
		}
		_, _ = io.WriteString(g, `{"type":"hello","version":1}`+"\n")
		if _, err := c.read(); err != nil {
			return
		}
		go func() { _, _ = io.Copy(io.Discard, g) }()
		for _, l := range lines {
			if _, err := io.WriteString(g, l+"\n"); err != nil {
				return
			}
		}
	}()
	return host
}

func TestNoExitCodeWithoutAnExitFrame(t *testing.T) {
	cases := map[string][]string{
		"closed":       {`{"type":"stdout","data":"aGk="}`},
		"no code":      {`{"type":"exit"}`},
		"malformed":    {`{"type":`},
		"unknown":      {`{"type":"surprise"}`},
		"no type":      {`{"data":"aGk="}`},
		"oversize":     {`{"type":"stdout","data":"` + strings.Repeat("QUFB", MaxChunk/3+1) + `"}`},
		"line too big": {`{"type":"stdout","message":"` + strings.Repeat("a", MaxLine) + `"}`},
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			code, err := Exec(context.Background(), fakeGuest(t, lines...), ExecRequest{Command: "x"}, Streams{Stdout: io.Discard})
			if err == nil {
				t.Fatalf("got exit code %d from a guest that sent none", code)
			}
		})
	}
}

func TestHelloMismatchIsRefused(t *testing.T) {
	host, g := net.Pipe()
	go func() {
		defer func() { _ = g.Close() }()
		c := newConn(g)
		_, _ = c.read()
		_, _ = io.WriteString(g, `{"type":"hello","version":9}`+"\n")
	}()
	if err := Ping(context.Background(), host); err == nil {
		t.Fatal("a guest speaking another version was accepted")
	}
}

func TestHybridConnect(t *testing.T) {
	for answer, ok := range map[string]bool{"OK 1073741824\n": true, "FAIL\n": false, "": false, strings.Repeat("O", 80): false} {
		host, vmm := net.Pipe()
		go func() {
			defer func() { _ = vmm.Close() }()
			buf := make([]byte, len("CONNECT 1024\n"))
			if _, err := io.ReadFull(vmm, buf); err != nil || string(buf) != "CONNECT 1024\n" {
				return
			}
			_, _ = io.WriteString(vmm, answer+`{"type":"hello"}`)
		}()
		err := HybridConnect(host, Port)
		if (err == nil) != ok {
			t.Fatalf("answer %q: %v", answer, err)
		}
		if ok {
			// Nothing past the answer line was consumed.
			b := make([]byte, 1)
			if _, err := host.Read(b); err != nil || b[0] != '{' {
				t.Fatalf("read past the answer: %q %v", b, err)
			}
		}
		_ = host.Close()
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
