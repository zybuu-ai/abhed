package egress

import (
	"net/netip"
	"testing"
	"time"
)

// fakeClock is a pool's clock that moves only when told.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func testPool(size uint32) (*synthPool, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	p := newSynthPool()
	p.now, p.size = clk.now, size
	return p, clk
}

// A name keeps its address while it is live, names never share one, and
// every address is inside the range, past its network address.
func TestSynthPoolMapsBothWays(t *testing.T) {
	p, _ := testPool(synthSize)
	a, fresh, ok := p.assign("a.test")
	if !ok || !fresh || !SynthPrefix.Contains(a) || a == SynthPrefix.Addr() {
		t.Fatalf("a.test got %s fresh=%v ok=%v", a, fresh, ok)
	}
	again, fresh, _ := p.assign("a.test")
	if again != a || fresh {
		t.Fatalf("a.test again got %s fresh=%v, want %s kept", again, fresh, a)
	}
	b, _, _ := p.assign("b.test")
	if b == a {
		t.Fatal("two names share an address")
	}
	if n, ok := p.lookup(a); !ok || n != "a.test" {
		t.Fatalf("lookup %s = %q %v", a, n, ok)
	}
	if _, ok := p.lookup(netip.MustParseAddr("198.18.200.1")); ok {
		t.Fatal("an address never given out maps to a name")
	}
	if last := offsetAddr(synthSize); !SynthPrefix.Contains(last) || last == netip.MustParseAddr("198.19.255.255") {
		t.Fatalf("the last address %s is outside the range or its broadcast", last)
	}
}

// A mapping ends synthHold after its name was last answered; answering it
// again keeps it, and an expired address may go to another name.
func TestSynthPoolExpiry(t *testing.T) {
	p, clk := testPool(2)
	a, _, _ := p.assign("a.test")
	clk.t = clk.t.Add(synthHold - time.Second)
	if _, _, ok := p.assign("a.test"); !ok { // refreshes
		t.Fatal("refresh failed")
	}
	clk.t = clk.t.Add(synthHold - time.Second)
	if n, ok := p.lookup(a); !ok || n != "a.test" {
		t.Fatal("a refreshed mapping expired early")
	}
	clk.t = clk.t.Add(2 * time.Second)
	if _, ok := p.lookup(a); ok {
		t.Fatal("an expired mapping still names its host")
	}
	a2, fresh, ok := p.assign("a.test")
	if !ok || !fresh {
		t.Fatalf("after expiry a.test got fresh=%v ok=%v", fresh, ok)
	}
	if n, _ := p.lookup(a2); n != "a.test" {
		t.Fatalf("new mapping names %q", n)
	}
}

// A full pool refuses a new name until a mapping expires, never handing
// out an address that still names another host.
func TestSynthPoolFull(t *testing.T) {
	p, clk := testPool(3)
	seen := map[netip.Addr]string{}
	for _, n := range []string{"a.test", "b.test", "c.test"} {
		a, _, ok := p.assign(n)
		if !ok {
			t.Fatalf("%s refused in a pool with room", n)
		}
		if prev, dup := seen[a]; dup {
			t.Fatalf("%s and %s share %s", prev, n, a)
		}
		seen[a] = n
	}
	if _, _, ok := p.assign("d.test"); ok {
		t.Fatal("a full pool gave out an address")
	}
	clk.t = clk.t.Add(synthHold)
	d, _, ok := p.assign("d.test")
	if !ok {
		t.Fatal("a pool of expired mappings refused a name")
	}
	if n, _ := p.lookup(d); n != "d.test" {
		t.Fatalf("reused %s names %q", d, n)
	}
}
