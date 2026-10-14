package container

import (
	"net/netip"
	"strconv"
	"strings"
)

// fallbackTunnels are the devices the kernel puts in every network namespace
// once the ipip, sit, ip6_tunnel, gre or vti modules are loaded. Down and
// with no address, one carries nothing.
var fallbackTunnels = map[string]bool{
	"tunl0": true, "sit0": true, "ip6tnl0": true, "gre0": true, "gretap0": true,
	"erspan0": true, "ip_vti0": true, "ip6_vti0": true, "ip6gre0": true,
}

// iffUp is IFF_UP in a device's flags.
const iffUp = 0x1

// netDevice is what the verify script read of one device in /sys/class/net.
type netDevice struct {
	oper  string
	flags uint64
	ok    bool
}

// networkWeakness reads the verify script's network lines for a container
// that should have no network: every device but lo is a weakness, except a
// kernel fallback tunnel that is down, not up, and holds no address.
func networkWeakness(lines []string) []string {
	var nets []string
	devs := map[string]netDevice{}
	inet6 := map[string]bool{}
	fibRead, v4 := false, []string{}
	last := ""
	for _, line := range lines {
		k, v, _ := strings.Cut(line, " ")
		v = strings.TrimSpace(v)
		switch k {
		case "net":
			if name, _, ok := strings.Cut(v, ":"); ok {
				nets = append(nets, strings.TrimSpace(name))
			}
		case "dev":
			f := strings.Fields(v)
			if len(f) != 3 {
				continue
			}
			flags, err := strconv.ParseUint(strings.TrimPrefix(f[2], "0x"), 16, 64)
			devs[f[0]] = netDevice{oper: f[1], flags: flags, ok: err == nil}
		case "fib-read":
			fibRead = true
		case "fib":
			// A local address is a "|-- a.b.c.d" line followed by "/32 host LOCAL".
			if a, ok := strings.CutPrefix(v, "|-- "); ok {
				last = a
			} else if strings.HasPrefix(v, "/32 host LOCAL") {
				if ip, err := netip.ParseAddr(last); err != nil || !ip.IsLoopback() {
					v4 = append(v4, last)
				}
			}
		case "inet6":
			if f := strings.Fields(v); len(f) == 6 {
				inet6[f[5]] = true
			}
		}
	}
	var weak []string
	for _, n := range nets {
		if n == "lo" {
			continue
		}
		d, seen := devs[n]
		idle := fallbackTunnels[n] && seen && d.ok && d.oper == "down" && d.flags&iffUp == 0 &&
			!inet6[n] && fibRead && len(v4) == 0
		if !idle {
			weak = append(weak, "has network device "+n)
		}
	}
	return weak
}
