package egress

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Decision is what the proxy did with a connection or request.
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
	// WouldDeny is a request the audit mode let through that the rules deny.
	WouldDeny Decision = "would_deny"
)

// Rule is one entry of the administrator's egress.rules.
type Rule struct {
	// Host is an exact name or address, or *.example.com for any name
	// under example.com (not example.com itself).
	Host string `json:"host"`
	// Ports are the ports the rule covers; empty means 80 and 443.
	Ports []int `json:"ports,omitempty"`
	// Methods and Paths narrow a rule to plain HTTP requests; empty means
	// any. A path is exact, or ends in /* for everything below it.
	Methods []string `json:"methods,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	// Decision is allow or deny. Deny rules win over allow rules.
	Decision string `json:"decision"`
	// AllowIPs are addresses or prefixes this rule may reach although they
	// are loopback, private or otherwise internal.
	AllowIPs []string `json:"allow_ips,omitempty"`
}

// Config is the egress section: the rules, the decision when none matches,
// and the mode.
type Config struct {
	Rules []Rule `json:"rules,omitempty"`
	// Default is deny (the default) or allow.
	Default string `json:"default,omitempty"`
	// Mode is enforce (the default) or audit, which lets a denied request
	// through and records it as would_deny.
	Mode string `json:"mode,omitempty"`
}

// defaultPorts are a rule's ports when it names none.
var defaultPorts = []uint16{80, 443}

type rule struct {
	name     string
	host     string // canonical; for a wildcard, the suffix with its leading dot
	wildcard bool
	ports    []uint16
	methods  []string
	paths    []string // exact, or a prefix ending in /
	prefix   []bool
	allow    bool
	allowIPs []netip.Prefix
}

// Policy is a compiled egress configuration.
type Policy struct {
	rules        []rule
	defaultAllow bool
	audit        bool
}

// Compile checks c and builds its policy.
func Compile(c Config) (*Policy, error) {
	p := &Policy{}
	switch c.Default {
	case "", "deny":
	case "allow":
		p.defaultAllow = true
	default:
		return nil, fmt.Errorf("egress.default is %q; use deny or allow", c.Default)
	}
	switch c.Mode {
	case "", "enforce":
	case "audit":
		p.audit = true
	default:
		return nil, fmt.Errorf("egress.mode is %q; use enforce or audit", c.Mode)
	}
	for i, r := range c.Rules {
		cr, err := compileRule(r)
		if err != nil {
			return nil, fmt.Errorf("egress.rules[%d]: %w", i, err)
		}
		cr.name = fmt.Sprintf("rules[%d] %s", i, r.Host)
		p.rules = append(p.rules, cr)
	}
	return p, nil
}

func compileRule(r Rule) (rule, error) {
	var out rule
	switch r.Decision {
	case "allow":
		out.allow = true
	case "deny":
	default:
		return out, fmt.Errorf("decision is %q; use allow or deny", r.Decision)
	}
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.Host)), ".")
	if rest, ok := strings.CutPrefix(h, "*."); ok {
		if !strings.Contains(rest, ".") {
			return out, fmt.Errorf("host %q is too wide: a wildcard needs at least two labels after *., as in *.example.com", r.Host)
		}
		c, err := CanonicalHost(rest)
		if err != nil || c != rest {
			return out, fmt.Errorf("host %q is not a name or *.name", r.Host)
		}
		if _, err := netip.ParseAddr(rest); err == nil {
			return out, fmt.Errorf("host %q puts a wildcard on an address", r.Host)
		}
		out.host, out.wildcard = "."+rest, true
	} else {
		c, err := CanonicalHost(strings.Trim(h, "[]"))
		if err != nil {
			return out, fmt.Errorf("host %q: %w", r.Host, err)
		}
		out.host = c
	}
	for _, p := range r.Ports {
		if p < 1 || p > 65535 {
			return out, fmt.Errorf("port %d is not 1 to 65535", p)
		}
		out.ports = append(out.ports, uint16(p)) // #nosec G115 -- checked above
	}
	if len(out.ports) == 0 {
		out.ports = defaultPorts
	}
	for _, m := range r.Methods {
		if m == "" || strings.Trim(m, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
			return out, fmt.Errorf("method %q is not an upper-case method name such as GET", m)
		}
		out.methods = append(out.methods, m)
	}
	for _, p := range r.Paths {
		if !strings.HasPrefix(p, "/") {
			return out, fmt.Errorf("path %q must start with /", p)
		}
		pre := false
		if s, ok := strings.CutSuffix(p, "/*"); ok {
			p, pre = s+"/", true
		}
		if strings.Contains(p, "*") {
			return out, fmt.Errorf("path %q: * is allowed only as a final /*", p)
		}
		if err := plainPath(p); err != nil {
			return out, fmt.Errorf("path %q: %w", p, err)
		}
		out.paths = append(out.paths, p)
		out.prefix = append(out.prefix, pre)
	}
	for _, s := range r.AllowIPs {
		pf, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil || a.Zone() != "" {
				return out, fmt.Errorf("allow_ips entry %q is not an address or prefix", s)
			}
			pf = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		out.allowIPs = append(out.allowIPs, pf.Masked())
	}
	return out, nil
}

// Request is what the proxy decides on: a tunnel has no method or path.
type Request struct {
	Host   string // canonical, as ParseTarget gives it
	Port   uint16
	Tunnel bool
	Method string
	Path   string // decoded
}

// Verdict is a decision and why.
type Verdict struct {
	Decision Decision
	// Rule names the rule that decided, or "default".
	Rule   string
	Reason string
	// AllowIPs are the internal addresses the allowing rules name.
	AllowIPs []netip.Prefix
}

// Allowed reports whether the request goes through.
func (v Verdict) Allowed() bool { return v.Decision != Deny }

// Audit reports whether the policy only records what it would deny.
func (p *Policy) Audit() bool { return p.audit }

// Rules is how many rules the policy holds.
func (p *Policy) Rules() int { return len(p.rules) }

// DefaultAllow reports whether a request no rule matches is allowed.
func (p *Policy) DefaultAllow() bool { return p.defaultAllow }

// Decide applies the rules to r: a matching deny rule wins, then a matching
// allow rule, then the default. In audit mode a deny becomes would_deny.
func (p *Policy) Decide(r Request) Verdict {
	var allowedBy []rule
	denied := ""
	for _, ru := range p.rules {
		if !ru.matches(r) {
			continue
		}
		if !ru.allow {
			if denied == "" {
				denied = ru.name
			}
			continue
		}
		allowedBy = append(allowedBy, ru)
	}
	var ips []netip.Prefix
	for _, ru := range allowedBy {
		ips = append(ips, ru.allowIPs...)
	}
	switch {
	case denied != "":
		v := p.deny(denied, "a deny rule matches")
		if v.Decision == WouldDeny {
			v.AllowIPs = ips
		}
		return v
	case len(allowedBy) > 0:
		return Verdict{Decision: Allow, Rule: allowedBy[0].name, Reason: "an allow rule matches", AllowIPs: ips}
	case p.defaultAllow:
		return Verdict{Decision: Allow, Rule: "default", Reason: "no rule matches and the default is allow"}
	}
	return p.deny("default", "no rule allows it")
}

func (p *Policy) deny(name, why string) Verdict {
	if p.audit {
		return Verdict{Decision: WouldDeny, Rule: name, Reason: why + "; let through in audit mode"}
	}
	return Verdict{Decision: Deny, Rule: name, Reason: why}
}

// matches reports whether the rule covers r. A tunnel's method and path are
// not seen, so a rule narrowed by them cannot allow one, and a deny rule
// narrowed by them refuses it: unverifiable, it is read the strict way.
func (ru rule) matches(r Request) bool {
	if !HostMatches(ru.host, ru.wildcard, r.Host) || !slices.Contains(ru.ports, r.Port) {
		return false
	}
	narrowed := len(ru.methods) > 0 || len(ru.paths) > 0
	if r.Tunnel {
		return !narrowed || !ru.allow
	}
	if len(ru.methods) > 0 && !slices.Contains(ru.methods, r.Method) {
		return false
	}
	if len(ru.paths) == 0 {
		return true
	}
	for i, p := range ru.paths {
		if r.Path == p || (ru.prefix[i] && (strings.HasPrefix(r.Path, p) || r.Path+"/" == p)) {
			return true
		}
	}
	return false
}

// HostMatches matches a canonical host against a rule's: exactly, or for a
// wildcard (suffix with its leading dot), any name ending in that suffix on
// a label boundary. *.example.com matches a.example.com, never example.com
// or example.com.evil.net.
func HostMatches(ruleHost string, wildcard bool, host string) bool {
	if !wildcard {
		return host == ruleHost
	}
	return len(host) > len(ruleHost) && strings.HasSuffix(host, ruleHost)
}

// errHostForm is a host that is not one plain spelling.
var errHostForm = errors.New("not a host name or address")
