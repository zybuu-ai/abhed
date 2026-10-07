package egress

import (
	"net/netip"
	"strings"
	"testing"
)

func mustCompile(t *testing.T, c Config) *Policy {
	t.Helper()
	p, err := Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWildcardMatchesOnALabelBoundary(t *testing.T) {
	p := mustCompile(t, Config{Rules: []Rule{{Host: "*.example.com", Decision: "allow"}}})
	for host, want := range map[string]bool{
		"a.example.com":         true,
		"a.b.example.com":       true,
		"example.com":           false,
		"example.com.evil.net":  false,
		"evilexample.com":       false,
		"a.example.com.evil":    false,
		"xexample.com":          false,
		"a.example.co":          false,
		"a.example.com.example": false,
	} {
		v := p.Decide(Request{Host: host, Port: 443, Tunnel: true})
		if v.Allowed() != want {
			t.Errorf("%s: allowed %v, want %v (%s)", host, v.Allowed(), want, v.Reason)
		}
	}
}

func TestExactHostAndPorts(t *testing.T) {
	p := mustCompile(t, Config{Rules: []Rule{
		{Host: "Example.COM.", Decision: "allow"},
		{Host: "api.example.net", Ports: []int{8443}, Decision: "allow"},
	}})
	cases := []struct {
		host string
		port uint16
		want bool
	}{
		{"example.com", 443, true},
		{"example.com", 80, true},
		{"example.com", 8080, false}, // no ports means 80 and 443
		{"www.example.com", 443, false},
		{"api.example.net", 8443, true},
		{"api.example.net", 443, false},
	}
	for _, c := range cases {
		if got := p.Decide(Request{Host: c.host, Port: c.port, Tunnel: true}).Allowed(); got != c.want {
			t.Errorf("%s:%d allowed %v, want %v", c.host, c.port, got, c.want)
		}
	}
}

func TestDenyWinsAndDefault(t *testing.T) {
	p := mustCompile(t, Config{Rules: []Rule{
		{Host: "*.example.com", Decision: "allow"},
		{Host: "secret.example.com", Decision: "deny"},
	}})
	v := p.Decide(Request{Host: "secret.example.com", Port: 443, Tunnel: true})
	if v.Decision != Deny || !strings.HasPrefix(v.Rule, "rules[1]") {
		t.Fatalf("deny rule did not win: %+v", v)
	}
	v = p.Decide(Request{Host: "other.net", Port: 443, Tunnel: true})
	if v.Decision != Deny || v.Rule != "default" {
		t.Fatalf("default is not deny: %+v", v)
	}
	open := mustCompile(t, Config{Default: "allow", Rules: []Rule{{Host: "bad.net", Decision: "deny"}}})
	if !open.Decide(Request{Host: "other.net", Port: 443, Tunnel: true}).Allowed() {
		t.Fatal("default allow refused")
	}
	if open.Decide(Request{Host: "bad.net", Port: 443, Tunnel: true}).Allowed() {
		t.Fatal("deny rule ignored under default allow")
	}
}

func TestMethodsAndPaths(t *testing.T) {
	p := mustCompile(t, Config{Rules: []Rule{
		{Host: "api.test", Ports: []int{80}, Methods: []string{"GET"}, Paths: []string{"/v1/*", "/health"}, Decision: "allow"},
		{Host: "api.test", Ports: []int{80}, Paths: []string{"/v1/admin/*"}, Decision: "deny"},
	}})
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v1/items", true},
		{"GET", "/v1", true},
		{"GET", "/v1/", true},
		{"GET", "/v10", false},
		{"GET", "/health", true},
		{"GET", "/health/x", false},
		{"POST", "/v1/items", false},
		{"GET", "/v1/admin/users", false},
		{"GET", "/", false},
		{"GET", "/v1/items;jsessionid=1", false},
	}
	for _, c := range cases {
		got := p.Decide(Request{Host: "api.test", Port: 80, Method: c.method, Path: c.path}).Allowed()
		if got != c.want {
			t.Errorf("%s %s allowed %v, want %v", c.method, c.path, got, c.want)
		}
	}
	// Deny rules on paths under an open host: a server that reads path
	// parameters routes /admin;x as /admin and /secret;x/a as /secret/a, so
	// a ; is refused before any rule is read.
	d := mustCompile(t, Config{Rules: []Rule{
		{Host: "api.test", Ports: []int{80}, Decision: "allow"},
		{Host: "api.test", Ports: []int{80}, Paths: []string{"/admin", "/secret/*"}, Decision: "deny"},
	}})
	for path, want := range map[string]bool{
		"/ok": true, "/admin": false, "/secret/a": false, "/admin;x": false, "/secret;x/a": false, "/ok;x": false,
	} {
		if got := d.Decide(Request{Host: "api.test", Port: 80, Method: "GET", Path: path}).Allowed(); got != want {
			t.Errorf("deny rules: GET %s allowed %v, want %v", path, got, want)
		}
	}
	// A tunnel's path is not seen: a narrowed allow cannot allow it, and a
	// narrowed deny refuses it. The narrowed allows alone, with no deny rule
	// to refuse the tunnel for them.
	n := mustCompile(t, Config{Rules: []Rule{
		{Host: "api.test", Ports: []int{80}, Methods: []string{"GET"}, Decision: "allow"},
		{Host: "api.test", Ports: []int{80}, Paths: []string{"/v1/*"}, Decision: "allow"},
	}})
	if v := n.Decide(Request{Host: "api.test", Port: 80, Tunnel: true}); v.Allowed() {
		t.Errorf("a method- or path-narrowed allow allowed a tunnel: %+v", v)
	}
	if !n.Decide(Request{Host: "api.test", Port: 80, Method: "GET", Path: "/x"}).Allowed() {
		t.Error("the narrowed allow refused a plain request it covers")
	}
	// Even under default allow, a ; is refused, and in audit mode too.
	for _, c := range []Config{{Default: "allow"}, {Default: "allow", Mode: "audit"}} {
		if v := mustCompile(t, c).Decide(Request{Host: "api.test", Port: 80, Method: "GET", Path: "/admin;x"}); v.Allowed() || v.Rule != "path" {
			t.Errorf("%+v: a ; in the path got %+v", c, v)
		}
	}
	q := mustCompile(t, Config{Rules: []Rule{
		{Host: "api.test", Decision: "allow"},
		{Host: "api.test", Paths: []string{"/admin/*"}, Decision: "deny"},
	}})
	if q.Decide(Request{Host: "api.test", Port: 443, Tunnel: true}).Allowed() {
		t.Error("a path-narrowed deny did not refuse a tunnel it cannot inspect")
	}
	if !q.Decide(Request{Host: "api.test", Port: 80, Method: "GET", Path: "/ok"}).Allowed() {
		t.Error("plain request outside the denied path refused")
	}
}

func TestAuditMode(t *testing.T) {
	p := mustCompile(t, Config{Mode: "audit", Rules: []Rule{{Host: "ok.test", Decision: "allow"}, {Host: "no.test", Decision: "deny"}}})
	for _, h := range []string{"no.test", "other.test"} {
		v := p.Decide(Request{Host: h, Port: 443, Tunnel: true})
		if v.Decision != WouldDeny || !v.Allowed() {
			t.Errorf("%s: %+v, want would_deny and let through", h, v)
		}
	}
	if v := p.Decide(Request{Host: "ok.test", Port: 443, Tunnel: true}); v.Decision != Allow {
		t.Errorf("allowed host in audit mode: %+v", v)
	}
}

func TestCompileRefuses(t *testing.T) {
	for name, c := range map[string]Config{
		"semicolon path": {Rules: []Rule{{Host: "a.test", Paths: []string{"/admin;x"}, Decision: "deny"}}},
		"negative idle":  {IdleSeconds: -1},
		"decision":       {Rules: []Rule{{Host: "a.test"}}},
		"wide wildcard":  {Rules: []Rule{{Host: "*.com", Decision: "allow"}}},
		"bare star":      {Rules: []Rule{{Host: "*", Decision: "allow"}}},
		"inner star":     {Rules: []Rule{{Host: "a.*.com", Decision: "allow"}}},
		"wildcard ip":    {Rules: []Rule{{Host: "*.1.2.3", Decision: "allow"}}},
		"numeric host":   {Rules: []Rule{{Host: "2130706433", Decision: "allow"}}},
		"port":           {Rules: []Rule{{Host: "a.test", Ports: []int{0}, Decision: "allow"}}},
		"method":         {Rules: []Rule{{Host: "a.test", Methods: []string{"get"}, Decision: "allow"}}},
		"path":           {Rules: []Rule{{Host: "a.test", Paths: []string{"v1"}, Decision: "allow"}}},
		"path dots":      {Rules: []Rule{{Host: "a.test", Paths: []string{"/a/../b"}, Decision: "allow"}}},
		"path star":      {Rules: []Rule{{Host: "a.test", Paths: []string{"/a*"}, Decision: "allow"}}},
		"allow_ips":      {Rules: []Rule{{Host: "a.test", AllowIPs: []string{"nope"}, Decision: "allow"}}},
		"default":        {Default: "maybe"},
		"mode":           {Mode: "warn"},
		"unicode host":   {Rules: []Rule{{Host: "bücher.test", Decision: "allow"}}},
		"space in host":  {Rules: []Rule{{Host: "a b.test", Decision: "allow"}}},
		"empty host":     {Rules: []Rule{{Host: "", Decision: "allow"}}},
		"ipv4-in-ipv6":   {Rules: []Rule{{Host: "::ffff:127.0.0.1", Decision: "allow"}}},
		"address w/zone": {Rules: []Rule{{Host: "fe80::1%en0", Decision: "allow"}}},
	} {
		if _, err := Compile(c); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
}

func TestAddrRefusal(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"fd00:ec2::254", "fe80::1", "224.0.0.1", "ff02::1", "0.0.0.0", "::", "100.64.0.1", "::ffff:127.0.0.1",
		"64:ff9b::a00:1", "fc00::1", "255.255.255.255"} {
		if AddrRefusal(netip.MustParseAddr(a)) == "" {
			t.Errorf("%s is not refused", a)
		}
	}
	for _, a := range []string{"93.184.216.34", "2606:4700::1111", "8.8.8.8"} {
		if why := AddrRefusal(netip.MustParseAddr(a)); why != "" {
			t.Errorf("%s refused: %s", a, why)
		}
	}
}

func TestAllowIPsReachTheRulesVerdict(t *testing.T) {
	p := mustCompile(t, Config{Rules: []Rule{{Host: "local.test", Ports: []int{8000}, Decision: "allow", AllowIPs: []string{"127.0.0.1", "10.0.0.0/8"}}}})
	v := p.Decide(Request{Host: "local.test", Port: 8000, Tunnel: true})
	if !v.Allowed() || !named(v.AllowIPs, netip.MustParseAddr("127.0.0.1")) || !named(v.AllowIPs, netip.MustParseAddr("10.9.8.7")) {
		t.Fatalf("allow_ips not carried: %+v", v)
	}
	if named(v.AllowIPs, netip.MustParseAddr("127.0.0.2")) {
		t.Fatal("a single address widened to more")
	}
}

func TestParseTarget(t *testing.T) {
	ok := []struct {
		method, target, host string
		port                 uint16
		path                 string
	}{
		{"CONNECT", "Example.com:443", "example.com", 443, ""},
		{"CONNECT", "[::1]:8443", "::1", 8443, ""},
		{"CONNECT", "example.com.:443", "example.com", 443, ""},
		{"GET", "http://example.com/a/b?q=1", "example.com", 80, "/a/b"},
		{"GET", "http://example.com", "example.com", 80, "/"},
		{"POST", "HTTP://EXAMPLE.com:8080/x%20y", "example.com", 8080, "/x y"},
	}
	for _, c := range ok {
		tg, err := ParseTarget(c.method, c.target)
		if err != nil {
			t.Errorf("%s %s: %v", c.method, c.target, err)
			continue
		}
		if tg.Host != c.host || tg.Port != c.port || tg.Path != c.path {
			t.Errorf("%s %s: got %s %d %q", c.method, c.target, tg.Host, tg.Port, tg.Path)
		}
	}
	bad := [][2]string{
		{"CONNECT", "example.com"},
		{"CONNECT", "example.com:0"},
		{"CONNECT", "example.com:0443"},
		{"CONNECT", "example.com:65536"},
		{"CONNECT", "2130706433:443"},
		{"CONNECT", "0x7f.1:443"},
		{"CONNECT", "[::ffff:127.0.0.1]:443"},
		{"CONNECT", "[fe80::1%25en0]:443"},
		{"GET", "/relative"},
		{"GET", "https://example.com/"},
		{"GET", "ftp://example.com/"},
		{"GET", "http://user:pw@example.com/"},
		{"GET", "http://example.com/a/../admin"},
		{"GET", "http://example.com/a/%2e%2e/admin"},
		{"GET", "http://example.com/a%2fb"},
		{"GET", "http://example.com//admin"},
		{"GET", "http://example.com/a\\b"},
		{"GET", "http://example.com:/"},
		{"get", "http://example.com/"},
		{"GET", "http://exa mple.com/"},
		{"GET", "http://bücher.test/"},
		{"GET", "http://example.com/admin;x"},
		{"GET", "http://example.com/secret;x/a"},
		{"GET", "http://example.com/admin%3Bx"},
	}
	for _, b := range bad {
		if _, err := ParseTarget(b[0], b[1]); err == nil {
			t.Errorf("%s %s parsed", b[0], b[1])
		}
	}
}

func TestParseRequestLine(t *testing.T) {
	tg, err := ParseRequestLine("CONNECT a.test:443 HTTP/1.1\r\n")
	if err != nil || tg.Host != "a.test" || !tg.Tunnel {
		t.Fatalf("%+v %v", tg, err)
	}
	for _, l := range []string{"CONNECT a.test:443", "CONNECT  a.test:443 HTTP/1.1", "CONNECT a.test:443 HTTP/2"} {
		if _, err := ParseRequestLine(l); err == nil {
			t.Errorf("%q parsed", l)
		}
	}
}
