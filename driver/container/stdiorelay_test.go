package container

import (
	"bytes"
	"fmt"
	"io"
	"net"
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
	go func() { served <- serveMux(in, toInside, fromInside) }()
	hostMux := make(chan error, 1)
	go func() {
		hostMux <- dialMux(toHost, fromHost, func() (net.Conn, error) { return net.Dial("tcp", up.Addr().String()) })
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
	go func() { _ = serveMux(in, toInside, fromInside) }()
	go func() {
		_ = dialMux(toHost, fromHost, func() (net.Conn, error) { return nil, io.ErrClosedPipe })
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
