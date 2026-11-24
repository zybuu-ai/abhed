// Package safeurl shows a server URL without what it may carry in secret:
// a user part (user:password, or a token as the user), query values (a
// token, a signature) and a fragment. Every surface that names an org
// server, a refusal, a warning, a status line or the record, goes through it.
package safeurl

import (
	"net/url"
	"strings"
)

// Server is the URL's scheme, host and path only, for text that reaches the
// model or the record: a refusal, a warning, a reason. A URL that does not
// parse as one with a host is "an unreadable server URL", never itself.
func Server(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		if strings.TrimSpace(raw) == "" {
			return ""
		}
		return "an unreadable server URL"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// Redacted is the whole URL with its user part, every query value and its
// fragment replaced by "redacted", for a status line where the shape of the
// URL helps; query names stay. A URL that does not parse is "an unreadable
// server URL".
func Redacted(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		if strings.TrimSpace(raw) == "" {
			return ""
		}
		return "an unreadable server URL"
	}
	if u.User != nil {
		u.User = url.User("redacted")
	}
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			q[k] = []string{"redacted"}
		}
		u.RawQuery = q.Encode()
	}
	if u.Fragment != "" || u.RawFragment != "" {
		u.Fragment, u.RawFragment = "redacted", ""
	}
	return u.String()
}
