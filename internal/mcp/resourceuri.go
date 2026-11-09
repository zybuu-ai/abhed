package mcp

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A resource URI is judged by policy as text, so only its one canonical
// spelling is read: a rule on docs://private/* must not be passed by
// DOCS://private/a, docs://public/../private/a or docs://private%2Fa, any of
// which a server may resolve to the same resource. A URI not in canonical
// form is refused, naming the form to write, and the server is sent exactly
// the URI policy judged.

// CanonicalResourceURI is uri's canonical form: scheme and authority in
// lower case, percent-encoded unreserved characters decoded and other
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
		authority := after[:end]
		if strings.Contains(authority, "@") {
			return "", errors.New("a resource URI carries no user information (@)")
		}
		out += "//" + strings.ToLower(authority)
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

// normalEscapes decodes escapes of unreserved characters and writes the
// rest in upper case; an encoded / or \ or NUL, or a bad escape, is refused.
func normalEscapes(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			return "", errors.New("a resource URI's % starts a two-digit hex escape")
		}
		v := unhex(s[i+1])<<4 | unhex(s[i+2])
		switch {
		case v == '/' || v == '\\' || v == 0:
			return "", errors.New("a resource URI holds no encoded /, \\ or NUL")
		case v >= 'a' && v <= 'z' || v >= 'A' && v <= 'Z' || v >= '0' && v <= '9' || v == '-' || v == '.' || v == '_' || v == '~':
			b.WriteByte(v)
		default:
			fmt.Fprintf(&b, "%%%02X", v)
		}
		i += 2
	}
	return b.String(), nil
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
