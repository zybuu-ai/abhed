package webfetch

import (
	"path"
	"strings"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// PathRuled reads deny and ask rules and reports, for a host, whether any
// web_fetch rule names a path on it: a rule whose path is more than a match
// of every path. A rule that is not a URL pattern, or whose host is a
// wildcard, counts for every host it could match.
func PathRuled(rules []string) func(host string) bool {
	var hosts []string // host patterns that carry a path rule; "*" for all
	for _, raw := range rules {
		r, err := policy.ParseRule(raw)
		if err != nil || !r.ForTool("web_fetch") {
			continue
		}
		open := strings.Index(raw, "(")
		if open < 0 {
			continue // the whole tool, whatever the path
		}
		glob := strings.TrimSpace(raw[open+1 : len(strings.TrimSpace(raw))-1])
		_, rest, ok := strings.Cut(glob, "://")
		if !ok {
			if glob != "*" && glob != "**" {
				hosts = append(hosts, "*")
			}
			continue
		}
		host, p, _ := strings.Cut(rest, "/")
		if h, _, found := strings.Cut(host, ":"); found {
			host = h
		}
		switch p {
		case "", "*", "**":
			continue
		}
		hosts = append(hosts, strings.ToLower(host))
	}
	return func(host string) bool {
		host = strings.ToLower(host)
		for _, h := range hosts {
			if h == "*" || h == host {
				return true
			}
			if strings.ContainsAny(h, "*?[") {
				if m, err := path.Match(h, host); err != nil || m {
					return true
				}
			}
		}
		return false
	}
}
