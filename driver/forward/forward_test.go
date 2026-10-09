//go:build !windows

package forward

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// reply reads everything the client sends, then answers and closes, so a
// stream that drops the reply on the client's half-close fails.
func reply(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 5)
				_, _ = io.ReadFull(c, b)
				_, _ = io.WriteString(c, "got:"+string(b))
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func relay(ctx context.Context, shell string, port int) *exec.Cmd {
	return exec.CommandContext(ctx, shell, "-c", RelayLine(port))
}

func TestRelayCarriesBothWays(t *testing.T) {
	port := reply(t)
	ctx, cancel := context.WithCancel(context.Background())
	closed := make(chan [2]int64, 1)
	s, err := Open(context.Background(), relay(ctx, "/bin/sh", port), port, cancel, func(in, out int64) { closed <- [2]int64{in, out} })
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(s, "hello")
	b, err := io.ReadAll(s)
	if err != nil || string(b) != "got:hello" {
		t.Fatalf("%q %v", b, err)
	}
	_ = s.Close()
	if got := <-closed; got != [2]int64{5, 9} {
		t.Fatalf("bytes %v", got)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("not done after close")
	}
}

func TestRelayRefusalsAreErrors(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	_, err := Open(context.Background(), relay(ctx, "/bin/sh", port), port, cancel, nil)
	if !errors.Is(err, driver.ErrUnavailable) {
		t.Fatalf("closed port: %v", err)
	}
	// A shell with neither tool on PATH.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cmd := relay(ctx2, "/bin/sh", port)
	cmd.Env = []string{"PATH=/nonexistent"}
	if _, err := Open(context.Background(), cmd, port, cancel2, nil); err == nil || !strings.Contains(err.Error(), "neither bash nor nc") {
		t.Fatalf("no tools: %v", err)
	}
	// A relay that never connects is given up on when the caller is.
	cctx, ccancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer ccancel()
	ctx3, cancel3 := context.WithCancel(context.Background())
	if _, err := Open(cctx, exec.CommandContext(ctx3, "/bin/sh", "-c", "sleep 30"), port, cancel3, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung relay: %v", err)
	}
}

func TestSetClosesEverythingAndRefusesAfter(t *testing.T) {
	var s Set
	a, b := net.Pipe()
	defer b.Close()
	if err := s.Add(a); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := a.Write([]byte("x")); err == nil {
		t.Fatal("a stream outlived the set")
	}
	c, d := net.Pipe()
	defer d.Close()
	if err := s.Add(c); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("added after close: %v", err)
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("a stream refused by a closed set was left open")
	}
}
