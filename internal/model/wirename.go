package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// validWireName is a tool name every provider accepts; only the request to
// the provider carries it, never policy or the record.
var validWireName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// wireHashLen is how many hex digits (64 bits) of the name's hash a wire name keeps.
const wireHashLen = 16

// ValidWireName reports whether name can be sent to a provider as it is.
func ValidWireName(name string) bool { return validWireName.MatchString(name) }

// WireName is name when every provider accepts it, else its plain characters
// up to 47 (mcp__<server>__ kept whole when it fits), "_" and 16 hex of its SHA-256.
func WireName(name string) string {
	if ValidWireName(name) {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	plain := strings.Map(func(r rune) rune {
		if r < 0x80 && (r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, name)
	if keep := 64 - 1 - wireHashLen; len(plain) > keep {
		plain = plain[:keep]
	}
	return plain + "_" + hex.EncodeToString(sum[:])[:wireHashLen]
}

// DefaultMaxTools is the tool limit of a profile that names none: the
// OpenAI-compatible limit.
const DefaultMaxTools = 128

// ToolLimit is the most tools one request to p may offer.
func ToolLimit(p Profile) int {
	if p.MaxTools > 0 {
		return p.MaxTools
	}
	return DefaultMaxTools
}

// ToolLimiter is a wrapping adapter that knows the limit of the provider
// that will serve its next request.
type ToolLimiter interface{ ToolLimit() int }

// ToolLimitOf is the tool limit of the provider a sends its next request to.
func ToolLimitOf(a Adapter) int {
	if l, ok := a.(ToolLimiter); ok {
		return l.ToolLimit()
	}
	return ToolLimit(a.Profile())
}

// ErrTooManyTools is a request that offers more tools than its provider takes.
var ErrTooManyTools = errors.New("too many tools for this provider")

// wireNames maps the wire names of one request back to the real names.
type wireNames map[string]string

// real is the tool name a provider's wire name stands for; a name the
// request never sent comes back as it is.
func (w wireNames) real(name string) string {
	if r, ok := w[name]; ok {
		return r
	}
	return name
}

// toWire renames req's tools and earlier calls by WireName, refusing more
// than limit tools or two names on one wire name; the map undoes it.
func toWire(req Request, limit int, provider string) (Request, wireNames, error) {
	if limit > 0 && len(req.Tools) > limit {
		return req, nil, fmt.Errorf("%w: this request offers %d tools and %s takes at most %d. "+
			"The built-in, extension and MCP tools offered in full already pass it; narrow an MCP server with allow_tools, "+
			"disable a server or extension, or set max_tools on the provider if it takes more", ErrTooManyTools, len(req.Tools), provider, limit)
	}
	var names wireNames
	need := false
	for _, t := range req.Tools {
		if !ValidWireName(t.Name) {
			need = true
			break
		}
	}
	for _, m := range req.Messages {
		for _, c := range m.ToolCalls {
			if !ValidWireName(c.Name) {
				need = true
			}
		}
	}
	if !need {
		return req, nil, nil
	}
	names = wireNames{}
	taken := map[string]string{}
	out := req
	out.Tools = make([]ToolDef, len(req.Tools))
	for i, t := range req.Tools {
		w := WireName(t.Name)
		if other, dup := taken[w]; dup && other != t.Name {
			return req, nil, fmt.Errorf("the tools %q and %q would reach the provider under one name, %s; rename one of them", other, t.Name, w)
		}
		taken[w] = t.Name
		if w != t.Name {
			names[w] = t.Name
		}
		t.Name = w
		out.Tools[i] = t
	}
	out.Messages = make([]Message, len(req.Messages))
	for i, m := range req.Messages {
		if len(m.ToolCalls) > 0 {
			calls := make([]ToolCall, len(m.ToolCalls))
			for j, c := range m.ToolCalls {
				// Every call is checked, a valid name too: it must not read as another tool's wire name.
				w := WireName(c.Name)
				if other, held := taken[w]; held && other != c.Name {
					return req, nil, fmt.Errorf("an earlier call to %q would reach the provider as %s, the name of the tool %q; rename one of them", c.Name, w, other)
				}
				taken[w] = c.Name
				if w != c.Name {
					names[w] = c.Name
					c.Name = w
				}
				calls[j] = c
			}
			m.ToolCalls = calls
		}
		out.Messages[i] = m
	}
	return out, names, nil
}

// fromWire relays a provider's stream with each call's real tool name.
func fromWire(in <-chan Chunk, names wireNames) <-chan Chunk {
	if len(names) == 0 {
		return in
	}
	out := make(chan Chunk, cap(in))
	go func() {
		defer close(out)
		for c := range in {
			if c.ToolCall != nil {
				tc := *c.ToolCall
				tc.Name = names.real(tc.Name)
				c.ToolCall = &tc
			}
			out <- c
		}
	}()
	return out
}
