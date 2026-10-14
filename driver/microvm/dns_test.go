//go:build !windows

package microvm

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

// The host passes each of the guest's lookups to the proxy's resolver with
// the sandbox's credential first, which the guest never sends or sees.
func TestServeDNSAddsTheSandboxCredential(t *testing.T) {
	sock := filepath.Join(shortTemp(t), "dns.sock")
	resolver, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	go func() {
		for {
			c, err := resolver.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				cred, err := readDNSFrame(c)
				if err != nil {
					return
				}
				q, err := readDNSFrame(c)
				if err != nil {
					return
				}
				_ = writeDNSFrame(c, append(append(cred, '|'), q...))
			}()
		}
	}()
	guestSide, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer guestSide.Close()
	d, _ := newFake(t)
	go d.serveDNS(guestSide, sock, "Basic c2FuZGJveDpkbnM=")
	c, err := net.Dial("tcp", guestSide.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	for _, q := range []string{"one", "two"} {
		if err := writeDNSFrame(c, []byte(q)); err != nil {
			t.Fatal(err)
		}
		a, err := readDNSFrame(c)
		if err != nil || string(a) != "Basic c2FuZGJveDpkbnM=|"+q {
			t.Fatalf("%s: %q %v", q, a, err)
		}
	}
}
