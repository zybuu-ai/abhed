//go:build !windows

package guest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPEM = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

// The CA bundle the host sends is written whole at the guest's CA file,
// readable by commands; a guest without a CA file refuses the call, as an
// older guest that does not know it does.
func TestPlaceCAWritesTheBundle(t *testing.T) {
	s := server(t)
	s.CAFile = filepath.Join(t.TempDir(), "abhed-ca.pem")
	bundle := []byte(strings.Repeat(testPEM, 2000)) // over one chunk
	if err := PlaceCA(context.Background(), pair(t, s), bundle); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(s.CAFile)
	if err != nil || !bytes.Equal(got, bundle) {
		t.Fatalf("the bundle written: %d bytes, %v", len(got), err)
	}
	if fi, _ := os.Stat(s.CAFile); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	// Placed again, it replaces the first.
	if err := PlaceCA(context.Background(), pair(t, s), []byte(testPEM)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(s.CAFile); string(got) != testPEM {
		t.Fatalf("not replaced: %d bytes", len(got))
	}
	if err := PlaceCA(context.Background(), pair(t, server(t)), []byte(testPEM)); err == nil {
		t.Fatal("a guest without a CA file took the bundle")
	}
	if err := PlaceCA(context.Background(), pair(t, s), []byte("not a certificate")); err == nil {
		t.Fatal("a bundle with no certificate was taken")
	}
}

// A guest bounds the bundle it reads, whatever a host sends.
func TestGuestBoundsTheCABundle(t *testing.T) {
	s := server(t)
	s.CAFile = filepath.Join(t.TempDir(), "abhed-ca.pem")
	host := pair(t, s)
	c, err := open(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.write(frame{Type: tCA}); err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), MaxChunk)
	var f frame
	go func() {
		for range MaxCABundle/MaxChunk + 2 {
			if c.write(frame{Type: tStdin, Data: chunk}) != nil {
				return
			}
		}
	}()
	if f, err = c.read(); err != nil || f.Type != tError || !strings.Contains(f.Message, "over") {
		t.Fatalf("an oversized bundle: %+v %v", f, err)
	}
	if _, err := os.Stat(s.CAFile); !os.IsNotExist(err) {
		t.Fatalf("a refused bundle left a file: %v", err)
	}
}
