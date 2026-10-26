package dapbridge

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
)

// The bridge carries stdin to the adapter and the adapter's answer to stdout.
func TestBridgeCarriesBothWays(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 64)
		n, _ := c.Read(buf)
		_, _ = c.Write(append([]byte("echo:"), buf[:n]...))
	}()
	var out, errs bytes.Buffer
	in := strings.NewReader("Content-Length: 2\r\n\r\n{}")
	if code := Run([]string{"--connect", ln.Addr().String(), "--wait", "2s"}, in, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if got := out.String(); got != "echo:Content-Length: 2\r\n\r\n{}" {
		t.Fatalf("stdout %q", got)
	}
}

// The bridge reaches only the sandbox's loopback, and gives up on an adapter
// that never listens.
func TestBridgeRefusals(t *testing.T) {
	for _, addr := range []string{"10.0.0.1:4711", "example.com:4711", "127.0.0.1", ""} {
		var errs bytes.Buffer
		if code := Run([]string{"--connect", addr}, strings.NewReader(""), io.Discard, &errs); code != 2 {
			t.Errorf("%q: exit %d: %s", addr, code, errs.String())
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	var errs bytes.Buffer
	if code := Run([]string{"--connect", addr, "--wait", "300ms"}, strings.NewReader(""), io.Discard, &errs); code != 1 ||
		!strings.Contains(errs.String(), "did not answer") {
		t.Fatalf("no adapter: exit %d: %s", code, errs.String())
	}
}
