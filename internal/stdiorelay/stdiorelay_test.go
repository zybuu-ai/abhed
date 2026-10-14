package stdiorelay

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// Connections made inside reach the host's dial through one pair of pipes,
// each in order and each closed in turn, and none is left open after.
func TestStdioMuxCarriesConnections(t *testing.T) {
	// The host's side: an upper-casing server, read to EOF, as the proxy would answer.
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				_, _ = c.Write(bytes.ToUpper(b))
			}()
		}
	}()
	in, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	toHost, fromInside := io.Pipe()
	toInside, fromHost := io.Pipe()
	served := make(chan error, 1)
	go func() { served <- Serve(in, toInside, fromInside) }()
	hostMux := make(chan error, 1)
	go func() {
		hostMux <- Dial(toHost, fromHost, func() (net.Conn, error) { return net.Dial("tcp", up.Addr().String()) }, nil)
	}()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", in.Addr().String())
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			msg := bytes.Repeat([]byte(fmt.Sprintf("conn %d ", i)), 5000)
			if _, err := c.Write(msg); err != nil {
				t.Error(err)
				return
			}
			closeWrite(c)
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			got, err := io.ReadAll(c)
			if err != nil || !bytes.Equal(got, bytes.ToUpper(msg)) {
				t.Errorf("conn %d: %d bytes back, %v", i, len(got), err)
			}
		}()
	}
	wg.Wait()
	// The host's stdio ending ends the inside.
	_ = fromHost.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the inside did not end with its stdin")
	}
	_ = fromInside.Close()
	select {
	case <-hostMux:
	case <-time.After(5 * time.Second):
		t.Fatal("the host end did not end")
	}
}

// A dial the host cannot make closes the connection inside.
func TestStdioMuxFailedDialClosesInside(t *testing.T) {
	in, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	toHost, fromInside := io.Pipe()
	toInside, fromHost := io.Pipe()
	go func() { _ = Serve(in, toInside, fromInside) }()
	go func() {
		_ = Dial(toHost, fromHost, func() (net.Conn, error) { return nil, io.ErrClosedPipe }, nil)
	}()
	defer fromHost.Close()
	c, err := net.Dial("tcp", in.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("read %d, %v", n, err)
	}
}

// openFrame writes the inside's open frame for id, as a misbehaving inside could.
func openFrame(t *testing.T, w io.Writer, id uint32) {
	t.Helper()
	var h [9]byte
	h[0] = frameOpen
	binary.BigEndian.PutUint32(h[1:5], id)
	if _, err := w.Write(h[:]); err != nil {
		t.Fatal(err)
	}
}

// hostEnd runs Dial against frames the test writes, counting the
// connections it makes and those still open.
func hostEnd(t *testing.T) (w io.Writer, dialled, open func() int) {
	t.Helper()
	fromInside, toHost := io.Pipe()
	var mu sync.Mutex
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Dial(fromInside, io.Discard, func() (net.Conn, error) {
			a, b := net.Pipe()
			go func() { _, _ = io.Copy(io.Discard, b) }()
			mu.Lock()
			conns = append(conns, b)
			mu.Unlock()
			return a, nil
		}, nil)
	}()
	t.Cleanup(func() { _ = toHost.Close(); <-done })
	dialled = func() int { mu.Lock(); defer mu.Unlock(); return len(conns) }
	open = func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, c := range conns {
			// A closed pipe end fails a write at once; an open one is read.
			_ = c.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
			if _, err := c.Write([]byte{0}); err == nil || !errors.Is(err, io.ErrClosedPipe) {
				n++
			}
		}
		return n
	}
	return toHost, dialled, open
}

// The host end opens at most maxStreams streams, however many the inside asks for.
func TestHostEndCapsOpenStreams(t *testing.T) {
	w, dialled, _ := hostEnd(t)
	for id := uint32(1); id <= maxStreams+50; id++ {
		openFrame(t, w, id)
	}
	deadline := time.Now().Add(5 * time.Second)
	for dialled() < maxStreams && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := dialled(); n != maxStreams {
		t.Fatalf("%d connections for %d opens, want %d", n, maxStreams+50, maxStreams)
	}
}

// An id opened twice closes the stream that had it and opens nothing new:
// no connection is left behind unreachable.
func TestHostEndRefusesARepeatedStreamID(t *testing.T) {
	w, dialled, open := hostEnd(t)
	openFrame(t, w, 7)
	openFrame(t, w, 7)
	openFrame(t, w, 8) // processed after both, so the first two are done
	deadline := time.Now().Add(5 * time.Second)
	for dialled() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := dialled(); n != 2 {
		t.Fatalf("%d connections dialled, want 2 (ids 7 and 8)", n)
	}
	if n := open(); n != 1 {
		t.Fatalf("%d connections open, want only id 8's", n)
	}
}

// The relay's user is never the commands'.
func TestRelayUserIsNotTheCommands(t *testing.T) {
	for _, uid := range []int{0, 501, 1000, 65533, 65534} {
		u := User(uid)
		if u == fmt.Sprintf("%d:%d", uid, uid) || strings.HasPrefix(u, fmt.Sprintf("%d:", uid)) {
			t.Errorf("uid %d: relay user %s", uid, u)
		}
	}
}
