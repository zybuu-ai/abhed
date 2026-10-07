package webfetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// blockedAddr says why an address is never fetched, or "" when it may be;
// the classes are the egress proxy's, so both refuse the same addresses.
func blockedAddr(a netip.Addr) string { return egress.AddrRefusal(a) }

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
		if err := t.addrCheck(host, ap); err != nil {
			return nil, err
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

// addrCheck refuses an internal address the tests have not permitted; under
// the allowlist it runs after the egress guard's own check.
func (t *Tool) addrCheck(host string, ap netip.AddrPort) error {
	if t.permit != nil && t.permit(ap) {
		return nil
	}
	if why := blockedAddr(ap.Addr()); why != "" {
		return &blockedError{host: host, addr: ap.Addr().Unmap(), why: why}
	}
	return nil
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
	// Written with /, %2F names another resource (GitLab's group%2Fproject),
	// so there is no spelling to suggest.
	if strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") {
		return nil, errors.New("the path has an encoded slash (%2F); such a URL cannot be fetched")
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
	if slices.Contains(labels, "") {
		return "", errors.New("the host has an empty part: two dots in a row, or more than one at the end")
	}
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
		// A server that applies NFKC reads fullwidth dots as dots, and one that
		// trims Unicode spaces drops a no-break space.
		// Format characters (zero-width space, soft hyphen, BOM) are invisible
		// and dropped here, so they cannot pad a segment that reads as dots.
		folded := strings.TrimFunc(norm.NFKC.String(strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, name)), unicode.IsSpace)
		if strings.Trim(folded, ". ") == "" || strings.ContainsAny(folded, "/\\") {
			return errors.New("the path has a segment that reads as . or .. or holds a slash " +
				"once normalised; write the path of the page itself")
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
