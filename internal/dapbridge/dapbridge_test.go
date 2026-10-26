package dapbridge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The bridge waits for the socket, then carries stdin to it and it to stdout.
func TestRunCarriesTheSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "dap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	go func() {
		time.Sleep(100 * time.Millisecond) // made after the bridge starts waiting
		ln, err := net.Listen("unix", sock)
		if err != nil {
			return
		}
		defer ln.Close()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err == nil {
			_, _ = c.Write(append([]byte("echo:"), buf...))
		}
	}()
	var out bytes.Buffer
	if err := Run("unix:"+sock, bytes.NewBufferString("hello"), &out, 5*time.Second); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if out.String() != "echo:hello" {
		t.Fatalf("out %q", out.String())
	}
	if err := Run("tcp:1.2.3.4:5", nil, nil, time.Millisecond); err == nil {
		t.Fatal("a TCP address was taken")
	}
}

// A pinned checksum must match; an unpinned (development) bridge is taken and said to be.
func TestCheckPins(t *testing.T) {
	p := filepath.Join(t.TempDir(), Name("arm64"))
	if err := os.WriteFile(p, []byte("bridge"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := PinnedArm64
	t.Cleanup(func() { PinnedArm64 = old })
	PinnedArm64 = ""
	if _, ok, err := check(p, "arm64"); err != nil || ok {
		t.Fatalf("unpinned: %v %v", ok, err)
	}
	sum := sha256.Sum256([]byte("bridge"))
	PinnedArm64 = hex.EncodeToString(sum[:])
	if _, ok, err := check(p, "arm64"); err != nil || !ok {
		t.Fatalf("pinned: %v %v", ok, err)
	}
	PinnedArm64 = "00"
	if _, _, err := check(p, "arm64"); err == nil {
		t.Fatal("a wrong checksum was taken")
	}
	if _, _, err := check(filepath.Join(filepath.Dir(p), "nope"), "arm64"); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing: %v", err)
	}
}
