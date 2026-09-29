package webfetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// blockedPrefixes are ranges no fetch may reach, beyond what netip's own
// predicates cover: shared, reserved and documentation space, and the IPv6
// forms that embed an IPv4 address a gateway would forward to.
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

// blockedAddr says why an address is never fetched, or "" when it may be.
func blockedAddr(a netip.Addr) string {
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

// blockedError is a refusal to reach an address, kept distinct from a
// network failure so the model is told it is a rule, not a fault to retry.
type blockedError struct {
	host string
	addr netip.Addr
	why  string
}

func (e *blockedError) Error() string {
	if e.host != "" && e.host != e.addr.String() {
		return fmt.Sprintf("%s resolves to %s, %s, which web_fetch never reaches", e.host, e.addr, e.why)
	}
	return fmt.Sprintf("%s is %s, which web_fetch never reaches", e.addr, e.why)
}

// dial resolves the host itself and connects only to an address it checked,
// so a name that resolves differently at connect time (DNS rebinding) is
// judged by the address actually dialled. Every hop of a redirect comes here.
func (t *Tool) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := net.LookupPort(network, portStr)
	if err != nil {
		return nil, err
	}
	addrs, err := t.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s has no address", host)
	}
	// Refused if any address is internal: a name that mixes public and private
	// addresses is not one to trust with the choice.
	for _, a := range addrs {
		ap := netip.AddrPortFrom(a.Unmap(), uint16(port)) // #nosec G115 -- LookupPort returns 0-65535
		if t.permit != nil && t.permit(ap) {
			continue
		}
		if why := blockedAddr(a); why != "" {
			return nil, &blockedError{host: host, addr: a.Unmap(), why: why}
		}
	}
	d := net.Dialer{Timeout: dialTimeout}
	var last error
	for _, a := range addrs {
		c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), uint16(port)).String()) // #nosec G115 -- as above
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

func (t *Tool) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, nil
	}
	if t.lookup != nil {
		return t.lookup(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// canonical is the one spelling of a URL that rules are matched against:
// lower-case scheme and host, no user info, no default port, no trailing dot,
// a path of at least "/", no needless escapes and no fragment. A URL written
// any other way is refused with this spelling, so a rule on a host cannot be
// stepped around by writing the host differently.
func canonical(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("not a URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		if u.Scheme == "" {
			return nil, errors.New("the URL needs a scheme: http:// or https://")
		}
		return nil, fmt.Errorf("the %s: scheme is not fetched; only http and https are", u.Scheme)
	}
	if u.Opaque != "" {
		return nil, errors.New("the URL has no host; write it as https://host/path")
	}
	if u.User != nil {
		return nil, errors.New("a URL with a user name or password is not fetched")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return nil, errors.New("the URL has no host")
	}
	for i := 0; i < len(host); i++ {
		if host[i] >= 0x80 {
			return nil, errors.New("write an international host name in its ASCII (xn--) form")
		}
	}
	host, err = canonicalHost(host)
	if err != nil {
		return nil, err
	}
	port, err := canonicalPort(u.Port())
	if err != nil {
		return nil, err
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	hostport := host
	if strings.Contains(host, ":") {
		hostport = "[" + host + "]"
	}
	if port != "" {
		hostport += ":" + port
	}
	if err := plainPath(u.Path); err != nil {
		return nil, err
	}
	out := &url.URL{Scheme: scheme, Host: hostport, Path: u.Path, RawQuery: u.RawQuery}
	if out.Path == "" {
		out.Path = "/"
	}
	return out, nil
}

// canonicalHost gives a host its one spelling. An address is written as
// netip writes it, and a name whose last label is a number is refused: the
// resolver reads 1572395042, 127.1 and 0x7f.1 as addresses, so each would be
// another spelling of an address a rule names.
func canonicalHost(host string) (string, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return "", errors.New("an address with a zone is not fetched")
		}
		if a.Is4In6() {
			return "", fmt.Errorf("write the address as %s, not as IPv6", a.Unmap())
		}
		return a.String(), nil
	}
	if strings.Contains(host, ":") {
		return "", errors.New("the host is not a valid address")
	}
	labels := strings.Split(host, ".")
	last := labels[len(labels)-1]
	if strings.HasPrefix(last, "0x") || strings.Trim(last, "0123456789") == "" {
		return "", errors.New("a host that ends in a number must be an IPv4 address written as four decimal numbers, such as 93.184.216.34")
	}
	return host, nil
}

// canonicalPort is the port as a plain number, 1 to 65535: 0443 is another
// spelling of 443 that a rule would not match.
func canonicalPort(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 || strings.Trim(p, "0123456789") != "" {
		return "", errors.New("the port must be a number from 1 to 65535")
	}
	return strconv.Itoa(n), nil
}

// hostAllowed applies the operator's allowlist: "example.com" names that host,
// "*.example.com" any host under it. An empty list allows every public host.
func hostAllowed(list []string, host string) bool {
	if len(list) == 0 {
		return true
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, h := range list {
		h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
		if rest, ok := strings.CutPrefix(h, "*."); ok {
			if strings.HasSuffix(host, "."+rest) {
				return true
			}
		} else if h == host {
			return true
		}
	}
	return false
}

// plainPath refuses a path a server would rewrite before acting on it: a .
// or .. segment, an empty one, or a backslash, decoded or not (u.Path is
// decoded). A rule on /admin must not be dodged by /public/../admin, and the
// refusal must not offer that spelling, so these are refused, not cleaned.
func plainPath(p string) error {
	if strings.Contains(p, "\\") {
		return errors.New("the path has a backslash; write it with / only")
	}
	if p == "" || p == "/" {
		return nil
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, seg := range segs {
		// A trailing slash leaves one empty last segment, which is ordinary.
		if seg == "" && i == len(segs)-1 {
			continue
		}
		// Some servers drop ;parameters before resolving, so /..;/ is .. to them.
		name, _, _ := strings.Cut(seg, ";")
		if name == "" || strings.Trim(name, ". ") == "" {
			return errors.New("the path has an empty, . or .. segment; write the path " +
				"of the page itself, with each folder named once")
		}
		// A control character, or an escape left after decoding, is how a
		// server that trims or decodes twice turns a segment into .. or /.
		lower := strings.ToLower(seg)
		if strings.ContainsFunc(seg, func(r rune) bool { return r < 0x20 || r == 0x7f }) ||
			strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
			return errors.New("the path has a control character or a doubly encoded . / or \\; " +
				"write the path of the page itself")
		}
	}
	return nil
}

// sameTarget reports whether a redirect may be followed without going back
// through policy: to the very same URL, or its upgrade from http to https on
// the default ports. Anything else, even on the same host, could be a path a
// rule judges differently.
func sameTarget(from, to *url.URL) bool {
	if to.User != nil || from.Hostname() != to.Hostname() ||
		from.EscapedPath() != to.EscapedPath() || from.RawQuery != to.RawQuery {
		return false
	}
	if from.Scheme == to.Scheme && from.Port() == to.Port() {
		return true
	}
	return from.Scheme == "http" && to.Scheme == "https" && from.Port() == "" && to.Port() == ""
}
