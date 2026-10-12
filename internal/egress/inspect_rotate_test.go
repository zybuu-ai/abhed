package egress

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// No session CA is valid for longer than two slices and an hour.
func TestSessionCALifetimeIsBounded(t *testing.T) {
	in, err := newInspector(nil)
	if err != nil {
		t.Fatal(err)
	}
	certs := parseBundle(t, in.pem)
	if len(certs) == 0 {
		t.Fatal("no CA")
	}
	for _, c := range certs {
		if life := c.NotAfter.Sub(c.NotBefore); life > 2*caSlice+2*time.Hour {
			t.Errorf("a session CA is valid for %v", life)
		}
	}
}

// After each slice the next CA signs, which a client that read the bundle
// before already trusts, and a new bundle file replaces the old whole.
func TestSessionCARotates(t *testing.T) {
	in, err := newInspector(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	in.now = func() time.Time { return now }
	p := &Proxy{ins: in}
	f, err := p.WriteCABundle(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	// A reader that opened the file before the rotation reads the old bundle whole.
	open, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = open.Close() }()
	before := readPool(t, f)
	first := in.signer
	verify(t, in, "a.example", before, now)

	now = now.Add(caSlice + time.Minute)
	verify(t, in, "a.example", before, now)
	if in.signer == first {
		t.Fatal("the CA did not rotate")
	}
	if got, _ := io.ReadAll(open); string(got) != string(old) {
		t.Fatal("a reader of the old bundle saw it change under it")
	}
	if after, _ := os.ReadFile(f); !strings.HasSuffix(string(after), string(p.CA())) {
		t.Fatal("the bundle's file was not replaced")
	}
	// The old file keeps a name, for a shared folder that cached it.
	if prev, _ := os.ReadFile(filepath.Join(filepath.Dir(f), ".ca-prev.pem")); string(prev) != string(old) {
		t.Fatal("the old bundle has no name left")
	}
	verify(t, in, "b.example", readPool(t, f), now)

	// Past the next slice too, as after a long sleep: the timer makes a fresh
	// pair and rewrites the file before a client reads it.
	now = now.Add(3 * caSlice)
	in.timer = time.AfterFunc(time.Hour, func() {})
	in.tick()
	in.stop()
	verify(t, in, "a.example", readPool(t, f), now)
}

func verify(t *testing.T, in *inspector, host string, roots *x509.CertPool, at time.Time) {
	t.Helper()
	c, err := in.leaf(host)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host, CurrentTime: at}); err != nil {
		t.Fatalf("%s at %v: %v", host, at, err)
	}
}

func readPool(t *testing.T, f string) *x509.CertPool {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(b)
	return pool
}

func parseBundle(t *testing.T, b []byte) []*x509.Certificate {
	t.Helper()
	var out []*x509.Certificate
	for _, blk := range splitPEM(b) {
		c, err := x509.ParseCertificate(blk)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func splitPEM(b []byte) [][]byte {
	var out [][]byte
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return out
		}
		out = append(out, blk.Bytes)
	}
}

// Readers during rotations only ever see a whole bundle: the system roots and
// two CAs.
func TestRotatedBundleIsNeverSeenInPart(t *testing.T) {
	in, err := newInspector(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	in.now = func() time.Time { return now }
	p := &Proxy{ins: in}
	f, err := p.WriteCABundle(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sys := len(withSystem(nil))
	done := make(chan struct{})
	bad := make(chan string, 1)
	go func() {
		for {
			select {
			case <-done:
				close(bad)
				return
			default:
			}
			b, err := os.ReadFile(f)
			if err != nil || len(b) < sys || len(splitPEM(b[sys:])) != 2 || !strings.HasSuffix(string(b), "-----END CERTIFICATE-----\n") {
				bad <- fmt.Sprintf("%d bytes, %v", len(b), err)
				close(bad)
				return
			}
		}
	}()
	for range 200 {
		now = now.Add(caSlice)
		in.mu.Lock()
		err := in.rotate()
		in.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	if why, ok := <-bad; ok {
		t.Fatalf("a reader saw part of a bundle: %s", why)
	}
}
