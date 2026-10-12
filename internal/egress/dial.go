package egress

import (
	"context"
	"net"
	"strings"
)

// Dial connects for one of Abhed's own clients that does not speak HTTP, such
// as ssh, judged by the guard in force as a tunnel to host:port is: by host and
// port, a rule narrowed by method or path never allowing it, and the address
// check after.
func Dial(ctx context.Context, kind, network, addr string) (net.Conn, error) {
	g := CallerOf(ctx).Guard
	if g == nil {
		var err error
		if g, err = Installed(); err != nil {
			return nil, err
		}
	}
	if g == nil || g == Unguarded {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return g.dialOwn(ctx, kind, network, addr)
}

// dialOwn judges, records and makes one tunnel-like connection.
func (g *Guard) dialOwn(ctx context.Context, kind, network, addr string) (net.Conn, error) {
	c := CallerOf(ctx)
	ev := Event{CallID: c.CallID, Kind: kind}
	host, ps, err := net.SplitHostPort(addr)
	if err == nil {
		host, err = CanonicalHost(strings.Trim(host, "[]"))
	}
	var port uint16
	if err == nil {
		port, err = parsePort(ps)
	}
	if err != nil {
		ev.Host, ev.Decision, ev.Rule, ev.Reason = addr, Deny, "parse", err.Error()
		g.record(c, ev)
		return nil, &DeniedError{Host: addr, Rule: ev.Rule, Reason: ev.Reason}
	}
	ev.Host, ev.Port = host, port
	var v Verdict
	if g.opts.Policy == nil {
		v = Verdict{Decision: Deny, Rule: "policy",
			Reason: "the egress policy could not be loaded (" + g.opts.Err.Error() + "), so only the model's endpoint is reached"}
	} else {
		v = g.opts.Policy.Decide(Request{Host: host, Port: port, Tunnel: true})
	}
	ev.Decision, ev.Rule, ev.Reason = v.Decision, v.Rule, v.Reason
	if !v.Allowed() {
		g.record(c, ev)
		return nil, &DeniedError{Host: host, Port: port, Rule: v.Rule, Reason: v.Reason}
	}
	ov := &ownVerdict{host: host, allowIPs: v.AllowIPs}
	conn, err := g.dial(context.WithValue(ctx, verdictKey{}, ov), network, net.JoinHostPort(host, ps))
	ov.mu.Lock()
	refused := ov.refused
	ov.mu.Unlock()
	if conn != nil {
		if ta, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
			ev.IP = ta.AddrPort().Addr().Unmap().String()
		}
	}
	switch {
	case refused != nil:
		ev.Decision, ev.Reason = Deny, refused.Error()
		g.record(c, ev)
		return nil, &DeniedError{Host: host, Port: port, Rule: v.Rule, Reason: refused.Error()}
	case err != nil:
		ev.Reason += "; the connection failed: " + err.Error()
	}
	g.record(c, ev)
	return conn, err
}

// Allow judges and records a connection a client makes itself, such as git
// pushing over HTTPS, before it is made: by host and port, as a tunnel.
func (g *Guard) Allow(ctx context.Context, kind, host string, port uint16) error {
	if g == nil || g == Unguarded {
		return nil
	}
	c := CallerOf(ctx)
	ev := Event{CallID: c.CallID, Kind: kind, Host: host, Port: port}
	h, err := CanonicalHost(host)
	if err != nil {
		ev.Decision, ev.Rule, ev.Reason = Deny, "parse", err.Error()
		g.record(c, ev)
		return &DeniedError{Host: host, Port: port, Rule: ev.Rule, Reason: ev.Reason}
	}
	ev.Host = h
	v := Verdict{Decision: Deny, Rule: "policy", Reason: "the egress policy could not be loaded"}
	if g.opts.Policy != nil {
		v = g.opts.Policy.Decide(Request{Host: h, Port: port, Tunnel: true})
	}
	ev.Decision, ev.Rule, ev.Reason = v.Decision, v.Rule, v.Reason
	g.record(c, ev)
	if !v.Allowed() {
		return &DeniedError{Host: h, Port: port, Rule: v.Rule, Reason: v.Reason}
	}
	return nil
}
