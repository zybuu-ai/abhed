//go:build !windows

package guest

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A dial joins the host to a port on the guest's loopback, with every byte
// the service sends kept, its first ones included; a closed port is an error.
func TestDialJoinsAGuestPort(t *testing.T) {
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
		_, _ = c.Write([]byte("banner\n"))
		b := make([]byte, 5)
		_, _ = io.ReadFull(c, b)
		_, _ = c.Write(bytes.ToUpper(b))
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	host := pair(t, server(t))
	if err := Dial(context.Background(), host, uint16(port)); err != nil { // #nosec G115 -- a port
		t.Fatal(err)
	}
	_ = host.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := host.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("banner\nHELLO"))
	if _, err := io.ReadFull(host, got); err != nil || string(got) != "banner\nHELLO" {
		t.Fatalf("through the dial: %q %v", got, err)
	}
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln2.Addr().(*net.TCPAddr).Port
	_ = ln2.Close()
	if err := Dial(context.Background(), pair(t, server(t)), uint16(closed)); err == nil { // #nosec G115 -- a port
		t.Fatal("a closed port answered")
	}
}

// An archive is unpacked into a folder of the workspace, its entries
// checked; a path out of the workspace, into its state or through a bad
// entry is refused.
func TestUnpackIntoTheWorkspace(t *testing.T) {
	s := server(t)
	tr, err := NewTracker(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Workspace = tr
	if err := os.Mkdir(filepath.Join(s.Dir, "in"), 0o755); err != nil {
		t.Fatal(err)
	}
	build := func(name, body string) *bytes.Buffer {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
		_ = tw.Close()
		return &b
	}
	if err := Unpack(context.Background(), pair(t, s), "in", build("src/a.txt", strings.Repeat("x", 100<<10))); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(s.Dir, "in", "src", "a.txt")); err != nil || len(b) != 100<<10 {
		t.Fatalf("unpacked: %d bytes, %v", len(b), err)
	}
	for _, c := range []struct{ dir, name string }{{"../x", "a"}, {".abhed", "a"}, {"in", "../escape"}, {"missing", "a"}} {
		if err := Unpack(context.Background(), pair(t, s), c.dir, build(c.name, "x")); err == nil {
			t.Errorf("%s with %s was not refused", c.dir, c.name)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.Dir), "escape")); err == nil {
		t.Fatal("an entry left the folder")
	}
	without := server(t)
	if err := Unpack(context.Background(), pair(t, without), ".", build("a", "x")); err == nil {
		t.Fatal("a guest without a workspace unpacked")
	}
}
