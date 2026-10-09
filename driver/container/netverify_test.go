package container

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// What the verify script prints of a --network none namespace on a host with
// the ipip module loaded (the real-engine probe: lo, and tunl0 down).
const (
	probeNet = "net Inter-|   Receive                                                |  Transmit\n" +
		"net face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		"net lo:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0\n" +
		"net tunl0:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0\n"
	probeDev  = "dev lo unknown 0x9\ndev tunl0 down 0x80\n"
	probeFib  = "fib-read\nfib Main:\nfib +-- 0.0.0.0/0 1 0 0\nfib |-- 127.0.0.0\nfib /8 host LOCAL\nfib |-- 127.0.0.1\nfib /32 host LOCAL\nfib |-- 127.255.255.255\nfib /32 link BROADCAST\n"
	probeV6   = "inet6 00000000000000000000000000000001 01 80 10 80       lo\n"
	probeIdle = probeNet + probeDev + probeFib + probeV6
)

func netLinesOf(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// A kernel fallback tunnel that is down and holds no address is no network;
// the same device up, addressed, or not described, still fails closed.
func TestNetworkWeaknessAcceptsOnlyIdleFallbackTunnels(t *testing.T) {
	if weak := networkWeakness(netLinesOf(probeIdle)); len(weak) != 0 {
		t.Fatalf("an idle tunl0 was refused: %q", weak)
	}
	all := probeNet + "net sit0: 0 0\nnet ip6tnl0: 0 0\nnet gre0: 0 0\nnet gretap0: 0 0\nnet erspan0: 0 0\nnet ip_vti0: 0 0\nnet ip6_vti0: 0 0\nnet ip6gre0: 0 0\n" +
		probeDev + "dev sit0 down 0x80\ndev ip6tnl0 down 0x88\ndev gre0 down 0x80\ndev gretap0 down 0x1002\ndev erspan0 down 0x1002\ndev ip_vti0 down 0x80\ndev ip6_vti0 down 0x80\ndev ip6gre0 down 0x0\n" +
		probeFib + probeV6
	if weak := networkWeakness(netLinesOf(all)); len(weak) != 0 {
		t.Fatalf("idle fallback tunnels were refused: %q", weak)
	}
	for name, report := range map[string]string{
		"tunnel up":                strings.Replace(probeIdle, "dev tunl0 down 0x80", "dev tunl0 down 0x81", 1),
		"tunnel operstate up":      strings.Replace(probeIdle, "dev tunl0 down 0x80", "dev tunl0 up 0x80", 1),
		"tunnel operstate unknown": strings.Replace(probeIdle, "dev tunl0 down 0x80", "dev tunl0 unknown 0x80", 1),
		"tunnel not in sysfs":      strings.Replace(probeIdle, "dev tunl0 down 0x80\n", "", 1),
		"tunnel flags unreadable":  strings.Replace(probeIdle, "dev tunl0 down 0x80", "dev tunl0 down", 1),
		"tunnel with IPv6":         probeIdle + "inet6 fe800000000000000000000000000001 05 40 20 80    tunl0\n",
		"an IPv4 address":          probeIdle + "fib |-- 10.0.0.5\nfib /32 host LOCAL\n",
		"fib_trie unread":          strings.Replace(probeIdle, "fib-read\n", "", 1),
		"another device down":      probeIdle + "net eth0: 0 0\ndev eth0 down 0x1002\n",
		"a lookalike name":         strings.ReplaceAll(probeIdle, "tunl0", "tunl1"),
	} {
		if weak := networkWeakness(netLinesOf(report)); len(weak) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Through Create: the verify script's real-engine output with tunl0 starts
// the container; tunl0 up removes it.
func TestVerifyAcceptsAnIdleTunnelDevice(t *testing.T) {
	for _, c := range []struct {
		report string
		ok     bool
	}{
		{probeIdle, true},
		{strings.Replace(probeIdle, "dev tunl0 down 0x80", "dev tunl0 up 0x81", 1), false},
	} {
		f := &fakeRunner{podman: true}
		head, _, _ := strings.Cut(healthy, "net ")
		f.respond = func(args []string) ([]byte, error, bool) {
			if args[0] == "exec" && slices.Contains(args, verifyScript) {
				return []byte(strings.Replace(head, "Uid: 501", "Uid: "+uidOf(args), 1) + c.report), nil, true
			}
			return nil, nil, false
		}
		d, ws := testDriver(t, f, Options{}, nil)
		_, err := d.Create(context.Background(), spec(ws))
		if (err == nil) != c.ok {
			t.Errorf("ok=%v: %v", c.ok, err)
		}
	}
}
