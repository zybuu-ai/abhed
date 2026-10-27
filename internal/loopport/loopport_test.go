package loopport

import (
	"errors"
	"net"
	"strconv"
	"testing"
)

func onMac(t *testing.T) {
	t.Helper()
	old := goos
	goos = "darwin"
	t.Cleanup(func() { goos = old })
}

func TestRange(t *testing.T) {
	t.Setenv(Env, "")
	if lo, hi := Range(); lo != DefaultLow || hi != DefaultHigh {
		t.Fatalf("default %d-%d", lo, hi)
	}
	for v, want := range map[string][2]int{"50000-50100": {50000, 50100}, "50000 - 50015": {50000, 50015},
		"80-200": {DefaultLow, DefaultHigh}, "50000-50010": {DefaultLow, DefaultHigh}, "x": {DefaultLow, DefaultHigh}} {
		t.Setenv(Env, v)
		if lo, hi := Range(); lo != want[0] || hi != want[1] {
			t.Errorf("%q: %d-%d", v, lo, hi)
		}
	}
}

// On a Mac a listener takes a port of the range on both stacks; with the
// range taken it fails clearly, never falling back to another port.
func TestListenInRangeAndExhausted(t *testing.T) {
	onMac(t)
	// A range of 16 ports nothing else is likely to use.
	base := 30520 // below the system's ephemeral ports
	t.Setenv(Env, strconv.Itoa(base)+"-"+strconv.Itoa(base+15))
	var held []net.Listener
	defer func() {
		for _, l := range held {
			_ = l.Close()
		}
	}()
	for range 16 {
		lns, err := ListenBoth(true)
		if err != nil {
			break // a port of the range taken by something else
		}
		p := lns[0].Addr().(*net.TCPAddr).Port
		if !In(p) {
			t.Fatalf("port %d outside the range", p)
		}
		if len(lns) == 2 && lns[1].Addr().(*net.TCPAddr).Port != p {
			t.Fatal("the two stacks differ")
		}
		held = append(held, lns...)
	}
	if _, err := ListenBoth(true); !errors.Is(err, ErrExhausted) {
		t.Fatalf("exhausted range: %v", err)
	}
	if _, err := Listen("127.0.0.1"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("exhausted range, one stack: %v", err)
	}
	// Ports for a command that holds its own are never of the range.
	ps, err := FreeOutside(5)
	if err != nil || len(ps) != 5 {
		t.Fatal(ps, err)
	}
	for _, p := range ps {
		if In(p) {
			t.Fatalf("%d is in the range", p)
		}
	}
}
