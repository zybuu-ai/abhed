//go:build !windows

package guest

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// The guest's resolver passes each query, over UDP or TCP, to the host as
// one framed exchange and returns the host's answer unchanged.
func TestServeDNSPassesQueriesToTheHost(t *testing.T) {
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	host := func() (io.ReadWriteCloser, error) {
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			q, err := readFrame(b)
			if err != nil {
				return
			}
			_ = writeFrame(b, append([]byte("answer:"), q...))
		}()
		return a, nil
	}
	go ServeDNS(udp, tcp, host)

	c, err := net.Dial("udp", udp.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write([]byte("q1"))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "answer:q1" {
		t.Fatalf("udp: %q %v", buf[:n], err)
	}

	tc, err := net.Dial("tcp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	for _, q := range []string{"q2", "q3"} {
		if err := writeFrame(tc, []byte(q)); err != nil {
			t.Fatal(err)
		}
		a, err := readFrame(tc)
		if err != nil || !bytes.Equal(a, []byte("answer:"+q)) {
			t.Fatalf("tcp %s: %q %v", q, a, err)
		}
	}
}

// A guest that was given no resolver refuses to start one.
func TestDNSRefusedWithoutAResolver(t *testing.T) {
	if err := DNS(t.Context(), pair(t, server(t))); err == nil {
		t.Fatal("a guest without a resolver started one")
	}
}
