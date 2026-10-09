//go:build linux

package microvm

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDialRefusesASymlinkedSocket(t *testing.T) {
	dir := shortTemp(t)
	real := filepath.Join(dir, "real.sock")
	l, err := net.Listen("unix", real)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	c, err := dialUnix(context.Background(), real)
	if err != nil {
		t.Fatalf("dialling the socket itself: %v", err)
	}
	_ = c.Close()

	link := filepath.Join(dir, "v.sock")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if c, err := dialUnix(context.Background(), link); err == nil {
		_ = c.Close()
		t.Fatal("followed a symlink the VMM could have planted")
	}
	file := filepath.Join(dir, "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := dialUnix(context.Background(), file); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("dialling a plain file: %v", err)
	}
}

func TestProbeFailsClosed(t *testing.T) {
	o := fcOptions(t)
	err := probeFirecracker(&o, *o.Firecracker)
	if err == nil {
		t.Fatal("the probe passed with no firecracker installed")
	}
	if os.Geteuid() != 0 && !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("as a user: %v", err)
	}
}

func TestTrustedBinary(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fc")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if err := trustedBinary(p); err == nil || !strings.Contains(err.Error(), "owned by root") {
			t.Fatalf("a binary another user owns: %v", err)
		}
	}
	_ = os.Chmod(p, 0o777)
	if err := trustedBinary(p); err == nil {
		t.Fatal("a world-writable binary was trusted")
	}
	if err := trustedBinary(p + ".missing"); err == nil {
		t.Fatal("a missing binary was trusted")
	}
}
