package egress

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Target is a parsed request target: a CONNECT authority, or an absolute
// http:// URL.
type Target struct {
	Host   string
	Port   uint16
	Tunnel bool
	Method string
	// Path is the decoded path of a plain HTTP request, without its query.
	Path string
	// URL is the plain request's URL, rewritten to its canonical host.
	URL *url.URL
}

// Request is the target as the policy decides on it.
func (t Target) Request() Request {
	return Request{Host: t.Host, Port: t.Port, Tunnel: t.Tunnel, Method: t.Method, Path: t.Path}
}

// Authority is host:port, with an IPv6 address in brackets.
func (t Target) Authority() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port)))
}

// maxTarget bounds a request target; anything longer is refused.
const maxTarget = 8192

// ParseRequestLine splits "METHOD target HTTP/1.x" and parses the target.
func ParseRequestLine(line string) (Target, error) {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	method, rest, ok1 := strings.Cut(line, " ")
	target, proto, ok2 := strings.Cut(rest, " ")
	if !ok1 || !ok2 {
		return Target{}, errors.New("malformed request line")
	}
	if proto != "HTTP/1.1" && proto != "HTTP/1.0" {
		return Target{}, fmt.Errorf("unsupported protocol %q", proto)
	}
	return ParseTarget(method, target)
}

// ParseTarget parses a request's method and target as a client of a
// forward proxy sends them. Anything that is not one plain spelling is
// refused rather than cleaned, so a rule cannot be stepped around by
// writing the host or path another way.
func ParseTarget(method, target string) (Target, error) {
	if method == "" || strings.Trim(method, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return Target{}, errors.New("the method is not an upper-case method name")
	}
	if target == "" || len(target) > maxTarget {
		return Target{}, errors.New("the request target is empty or too long")
	}
	for i := 0; i < len(target); i++ {
		if c := target[i]; c <= 0x20 || c >= 0x7f {
			return Target{}, errors.New("the request target has a space, control or non-ASCII character")
		}
	}
	if method == "CONNECT" {
		return parseAuthority(target)
	}
	return parseAbsolute(method, target)
}

func parseAuthority(a string) (Target, error) {
	host, portStr, err := net.SplitHostPort(a)
	if err != nil {
		return Target{}, errors.New("CONNECT needs host:port")
	}
	h, err := CanonicalHost(host)
	if err != nil {
		return Target{}, err
	}
	port, err := parsePort(portStr)
	if err != nil {
		return Target{}, err
	}
	return Target{Host: h, Port: port, Tunnel: true, Method: "CONNECT"}, nil
}

func parseAbsolute(method, raw string) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, errors.New("the request target is not a URL")
	}
	if !strings.EqualFold(u.Scheme, "http") {
		if strings.EqualFold(u.Scheme, "https") {
			return Target{}, errors.New("send https through CONNECT, not as an absolute https:// URL")
		}
		return Target{}, errors.New("a forward proxy request needs an absolute http:// URL")
	}
	if u.Opaque != "" || u.User != nil || u.Fragment != "" || u.Host == "" {
		return Target{}, errors.New("the URL must be http://host[:port]/path, without user info or fragment")
	}
	h, err := CanonicalHost(u.Hostname())
	if err != nil {
		return Target{}, err
	}
	port := uint16(80)
	if p := u.Port(); p != "" {
		if port, err = parsePort(p); err != nil {
			return Target{}, err
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return Target{}, errors.New("the URL has an empty port")
	}
	lower := strings.ToLower(u.EscapedPath())
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "%2e") || strings.Contains(lower, "%00") {
		return Target{}, errors.New("the path has an encoded slash, backslash, dot or NUL")
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	if err := plainPath(path); err != nil {
		return Target{}, err
	}
	out := &url.URL{Scheme: "http", Host: net.JoinHostPort(h, strconv.Itoa(int(port))), Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}
	if port == 80 {
		out.Host = h
		if strings.Contains(h, ":") {
			out.Host = "[" + h + "]"
		}
	}
	return Target{Host: h, Port: port, Method: method, Path: path, URL: out}, nil
}

func parsePort(p string) (uint16, error) {
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 || strings.Trim(p, "0123456789") != "" || strconv.Itoa(n) != p {
		return 0, errors.New("the port must be a number from 1 to 65535, written plainly")
	}
	return uint16(n), nil // #nosec G115 -- checked above
}

// CanonicalHost gives a host its one spelling: lower case, no trailing dot,
// an address as netip writes it. A name whose last label is a number is
// refused, as resolvers read 2130706433 or 0x7f.1 as addresses; so is a
// non-ASCII name (write its xn-- form), an address with a zone, and an
// IPv4 address written as IPv6.
func CanonicalHost(host string) (string, error) {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" || len(h) > 253 {
		return "", errHostForm
	}
	if a, err := netip.ParseAddr(h); err == nil {
		if a.Zone() != "" {
			return "", errors.New("an address with a zone is not reached")
		}
		if a.Is4In6() {
			return "", fmt.Errorf("write the address as %s, not as IPv6", a.Unmap())
		}
		return a.String(), nil
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return "", errors.New("the host has an empty or over-long part")
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return "", errors.New("the host has a character other than a-z, 0-9, - or _ (write an international name in its xn-- form)")
			}
		}
	}
	last := labels[len(labels)-1]
	if strings.HasPrefix(last, "0x") || strings.Trim(last, "0123456789") == "" {
		return "", errors.New("a host that ends in a number must be an IPv4 address written as four decimal numbers")
	}
	return h, nil
}

// plainPath refuses a decoded path a server would rewrite before acting on
// it: a . or .. segment, an empty one, a backslash, a control character or
// a ;. A rule on /admin must not be dodged by /public/../admin, nor by
// /admin;x, which servers that read path parameters route as /admin.
func plainPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return errors.New("the path must start with /")
	}
	if strings.Contains(p, ";") {
		return errSemicolon
	}
	if strings.ContainsAny(p, "\\") || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errors.New("the path has a backslash or control character")
	}
	if p == "/" {
		return nil
	}
	segs := strings.Split(p[1:], "/")
	for i, seg := range segs {
		if seg == "" && i == len(segs)-1 {
			continue
		}
		if seg == "" || strings.Trim(seg, ". ") == "" {
			return errors.New("the path has an empty, . or .. segment")
		}
	}
	return nil
}
