package egress

import "testing"

// A host a person let commands reach goes through on 80 and 443; the whole
// network, once opened, lets any host through; a deny rule wins over both.
func TestGrantHostsAndAny(t *testing.T) {
	p, err := Compile(Config{Rules: []Rule{{Host: "evil.example", Decision: "deny"}}})
	if err != nil {
		t.Fatal(err)
	}
	tunnel := func(h string, port uint16) Decision {
		return p.DecideHost(Request{Host: h, Port: port, Tunnel: true}).Decision
	}
	if tunnel("github.com", 443) != Deny || p.DecideName("github.com").Decision != Deny {
		t.Fatal("open before any grant")
	}
	if err := p.GrantHosts("network", []string{"github.com"}); err != nil {
		t.Fatal(err)
	}
	if tunnel("github.com", 443) != Allow || tunnel("github.com", 80) != Allow || tunnel("api.github.com", 443) != Deny {
		t.Fatal("the granted host")
	}
	if p.Decide(Request{Host: "github.com", Port: 80, Method: "GET", Path: "/"}).Decision != Allow {
		t.Fatal("plain http to the granted host")
	}
	if err := p.GrantHosts("network", []string{"*.example.com"}); err == nil {
		t.Fatal("a wildcard was granted")
	}
	if p.AnyGranted() {
		t.Fatal("any before it was granted")
	}
	p.GrantAny("network for this workspace")
	if !p.AnyGranted() || tunnel("pypi.org", 443) != Allow || p.DecideName("pypi.org").Decision != Allow {
		t.Fatal("the opened network")
	}
	if v := p.DecideHost(Request{Host: "pypi.org", Port: 443, Tunnel: true}); v.Rule != "network for this workspace" {
		t.Fatalf("named %q", v.Rule)
	}
	if tunnel("evil.example", 443) != Deny || p.Decide(Request{Host: "evil.example", Port: 80, Method: "GET", Path: "/"}).Decision != Deny {
		t.Fatal("a deny rule lost to the grant")
	}
}
