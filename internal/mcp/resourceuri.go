package mcp

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// A resource URI is judged by policy as text, so only its one canonical
// spelling is read: a rule on docs://private/* must not be passed by
// DOCS://private/a, docs://public/../private/a or docs://private%2Fa, any of
// which a server may resolve to the same resource. A URI not in canonical
// form is refused, naming the form to write, and the server is sent exactly
// the URI policy judged.

// CanonicalResourceURI is uri's canonical form: scheme and authority in
// lower case (see canonicalAuthority), escaped unreserved characters decoded and other
// escapes in upper case, no dot-segments and no empty path segments. It
// refuses what has none: control or space characters, an encoded / or \,
// a backslash, a fragment, user information, and a rooted path without an
// authority (scheme:/x, which is neither scheme:x nor scheme:///x).
func CanonicalResourceURI(uri string) (string, error) {
	if uri == "" || len(uri) > MaxResourceURI || !utf8.ValidString(uri) {
		return "", fmt.Errorf("a resource URI is up to %d characters of UTF-8", MaxResourceURI)
	}
	for _, r := range uri {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == unicode.ReplacementChar || unicode.Is(unicode.Cf, r) {
			return "", errors.New("a resource URI holds no spaces or control characters; write a space as %20")
		}
	}
	if strings.ContainsAny(uri, `\#`) {
		return "", errors.New("a resource URI holds no backslash and no fragment (#)")
	}
	scheme, rest, ok := strings.Cut(uri, ":")
	if !ok || scheme == "" {
		return "", errors.New("a resource URI starts with a scheme, as docs:// or file://")
	}
	for i, r := range scheme {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (i == 0 || (r < '0' || r > '9') && r != '+' && r != '-' && r != '.') {
			return "", errors.New("a resource URI's scheme is letters, digits, +, - or dots")
		}
	}
	rest, err := normalEscapes(rest)
	if err != nil {
		return "", err
	}
	out := strings.ToLower(scheme) + ":"
	tail := rest
	if after, ok := strings.CutPrefix(rest, "//"); ok {
		end := strings.IndexAny(after, "/?")
		if end < 0 {
			end = len(after)
		}
		authority, err := canonicalAuthority(strings.ToLower(scheme), after[:end])
		if err != nil {
			return "", err
		}
		out += "//" + authority
		tail = after[end:]
	} else if strings.HasPrefix(rest, "/") {
		return "", fmt.Errorf("write %s:///%s or %s:%s; a rooted path needs an authority", strings.ToLower(scheme), strings.TrimLeft(rest, "/"), strings.ToLower(scheme), strings.TrimLeft(rest, "/"))
	}
	path, query, hasQuery := strings.Cut(tail, "?")
	if strings.Contains(path, "/") {
		path = cleanPath(path)
	}
	out += path
	if hasQuery {
		out += "?" + query
	}
	return out, nil
}

// canonicalAuthority is an authority in its one spelling: lower case, the
// host with no trailing dot, no default port for http and https, and no
// localhost for file. A host not in ASCII is refused: written in its
// punycode form, it has one spelling, where Unicode has many.
func canonicalAuthority(scheme, a string) (string, error) {
	if strings.Contains(a, "@") {
		return "", errors.New("a resource URI carries no user information (@)")
	}
	for i := 0; i < len(a); i++ {
		if a[i] >= 0x80 || a[i] == '%' {
			return "", errors.New("a resource URI's host is ASCII: write an international name in its punycode (xn--) form")
		}
	}
	a = strings.ToLower(a)
	host, port := a, ""
	if strings.HasPrefix(a, "[") {
		end := strings.IndexByte(a, ']')
		if end < 0 {
			return "", errors.New("a resource URI's [ host has no ]")
		}
		host, port = a[:end+1], a[end+1:]
		if port != "" && !strings.HasPrefix(port, ":") {
			return "", errors.New("a resource URI's ] host is followed by a port or nothing")
		}
		port = strings.TrimPrefix(port, ":")
		// An IPv6 literal in its one spelling; an IPv4 one is written without brackets.
		ip := net.ParseIP(host[1:end])
		if ip == nil || ip.To4() != nil {
			return "", errors.New("a resource URI's [ ] host is an IPv6 address")
		}
		host = "[" + ip.String() + "]"
	} else if i := strings.LastIndexByte(a, ':'); i >= 0 {
		host, port = a[:i], a[i+1:]
	}
	host = strings.TrimRight(host, ".")
	// A host that ends in a number is an IPv4 address in its dotted-quad form:
	// 127.1 and 0x7f.0.0.1 are other spellings of one.
	if labels := strings.Split(host, "."); !strings.HasPrefix(host, "[") {
		last := labels[len(labels)-1]
		if last != "" && (strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x")) {
			if ip := net.ParseIP(host); ip == nil || ip.To4() == nil || ip.String() != host {
				return "", errors.New("a resource URI's numeric host is an IPv4 address written as four decimal numbers")
			}
		}
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strings.Trim(port, "0123456789") != "" {
			return "", errors.New("a resource URI's port is a number from 1 to 65535")
		}
		port = strconv.Itoa(n)
		if scheme == "https" && n == 443 || scheme == "http" && n == 80 {
			port = ""
		}
	}
	if scheme == "file" && host == "localhost" && port == "" {
		host = ""
	}
	if port != "" {
		return host + ":" + port, nil
	}
	return host, nil
}

// normalEscapes decodes escapes of unreserved characters, and escapes that
// spell printable UTF-8, which then has one form, NFC; the rest are written
// in upper case. An encoded / or \ or NUL, or a bad escape, is refused.
func normalEscapes(s string) (string, error) {
	var b strings.Builder
	var high []byte // escaped bytes of 0x80 and above, decoded together
	flush := func() {
		for len(high) > 0 {
			r, n := utf8.DecodeRune(high)
			if r != utf8.RuneError && printableRune(r) {
				b.WriteString(string(r))
			} else {
				for _, c := range high[:max(n, 1)] {
					fmt.Fprintf(&b, "%%%02X", c)
				}
			}
			high = high[max(n, 1):]
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			flush()
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			return "", errors.New("a resource URI's % starts a two-digit hex escape")
		}
		v := unhex(s[i+1])<<4 | unhex(s[i+2])
		i += 2
		if v >= 0x80 {
			high = append(high, v)
			continue
		}
		flush()
		switch {
		case v == '/' || v == '\\' || v == 0:
			return "", errors.New("a resource URI holds no encoded /, \\ or NUL")
		case v >= 'a' && v <= 'z' || v >= 'A' && v <= 'Z' || v >= '0' && v <= '9' || v == '-' || v == '.' || v == '_' || v == '~':
			b.WriteByte(v)
		default:
			fmt.Fprintf(&b, "%%%02X", v)
		}
	}
	flush()
	return norm.NFC.String(b.String()), nil
}

// printableRune is a character a URI may hold as written: not a control,
// space or format character.
func printableRune(r rune) bool {
	return !unicode.IsControl(r) && !unicode.IsSpace(r) && !unicode.Is(unicode.Cf, r) && r != unicode.ReplacementChar
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// cleanPath removes dot-segments (RFC 3986 §5.2.4) and empty segments,
// keeping a leading and a trailing slash.
func cleanPath(p string) string {
	lead, trail := strings.HasPrefix(p, "/"), strings.HasSuffix(p, "/") && len(p) > 1
	var out []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	last := ""
	if segs := strings.Split(p, "/"); len(segs) > 0 {
		last = segs[len(segs)-1]
	}
	if last == "." || last == ".." {
		trail = len(out) > 0
	}
	r := strings.Join(out, "/")
	if lead {
		r = "/" + r
	}
	if trail && r != "/" {
		r += "/"
	}
	return r
}
