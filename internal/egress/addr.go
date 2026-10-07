package egress

import "net/netip"

// blockedPrefixes are ranges never reached unless a rule names them, beyond
// what netip's own predicates cover: shared, reserved and documentation
// space, and the IPv6 forms that embed an IPv4 address a gateway would
// forward to.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",          // "this network"
		"100.64.0.0/10",      // carrier-grade NAT, often internal
		"192.0.0.0/24",       // IETF protocol assignments
		"192.0.2.0/24",       // documentation
		"198.18.0.0/15",      // benchmarking
		"198.51.100.0/24",    // documentation
		"203.0.113.0/24",     // documentation
		"240.0.0.0/4",        // reserved, and the broadcast address
		"64:ff9b::/96",       // NAT64: reaches any IPv4 address, private ones too
		"64:ff9b:1::/48",     // local-use NAT64
		"100::/64",           // discard
		"2001::/32",          // Teredo
		"2001:db8::/32",      // documentation
		"2002::/16",          // 6to4
		"fec0::/10",          // site-local, deprecated but still routed in places
		"169.254.169.254/32", // cloud metadata; link-local covers it, named for the reader
		"fd00:ec2::254/128",  // the EC2 metadata service over IPv6
		"::/96",              // IPv4-compatible: ::127.0.0.1 and the like
		"::ffff:0:0:0/96",    // SIIT: translates to an IPv4 address
		"192.88.99.0/24",     // 6to4 relay anycast
		"2001:10::/28",       // ORCHID
		"2001:20::/28",       // ORCHIDv2
		"3fff::/20",          // documentation
		"5f00::/16",          // segment routing
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// AddrRefusal says why an address is not reached unless a rule names it, or
// "" when it may be: loopback, private, link-local, cloud metadata,
// multicast, unspecified, and reserved or internal ranges.
func AddrRefusal(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case !a.IsValid():
		return "not an address"
	case a.IsLoopback():
		return "a loopback address"
	case a.IsPrivate():
		return "a private address"
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return "a link-local address"
	case a.IsUnspecified():
		return "an unspecified address"
	case a.IsMulticast(), a.IsInterfaceLocalMulticast():
		return "a multicast address"
	case !a.IsGlobalUnicast():
		return "not a public unicast address"
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return "a reserved or internal address"
		}
	}
	return ""
}
