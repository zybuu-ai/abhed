package seccomp

import "testing"

// socketcall(SOCKET) is refused on s390x unless the network is on and the
// setting asks for it; it never applies where the call does not exist.
func TestSocketcallFollowsTheSetting(t *testing.T) {
	has := func(p Policy, arch string) bool {
		for _, r := range p.rules(arches[arch]) {
			if r.name == "socketcall" {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		p    Policy
		want bool
	}{
		{Policy{Abhed: 9}, true},
		{Policy{Abhed: 9, AllowNetwork: true}, true},
		{Policy{Abhed: 9, Socketcall: true}, true},
		{Policy{Abhed: 9, AllowNetwork: true, Socketcall: true}, false},
	} {
		if got := has(c.p, "s390x"); got != c.want {
			t.Errorf("%+v: refused %v, want %v", c.p, got, c.want)
		}
		if _, err := c.p.Program("s390x"); err != nil {
			t.Errorf("%+v: %v", c.p, err)
		}
	}
	if has(Policy{Abhed: 9}, "amd64") {
		t.Error("a socketcall rule on amd64")
	}
}

// Under the egress allowlist only inet and inet6 stream sockets are made.
func TestProxyOnlyAllowsStreamsAlone(t *testing.T) {
	p := Policy{Abhed: 9, ProxyOnly: true}
	for _, arch := range []string{"amd64", "arm64", "s390x"} {
		prog, err := p.Program(arch)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		nr := arches[arch].nr["socket"]
		for _, c := range []struct {
			family, typ, proto uint32
			allow              bool
		}{{2, 1, 0, true}, {10, 1 | 0x80000, 0, true}, {2, 1, 6, true}, {10, 1, 6, true}, {2, 2, 0, false}, {10, 2, 0, false},
			{2, 3, 0, false}, {1, 1, 0, false}, {16, 3, 0, false}, {2, 5, 0, false},
			{2, 1, 262, false}, {10, 1, 262, false}, {2, 1, 132, false}, {10, 1 | 0x800, 132, false}, {2, 1, 17, false}} {
			got := run(t, prog, call{arch: arches[arch].audit, nr: nr, args: [6]uint64{uint64(c.family), uint64(c.typ), uint64(c.proto)}}) == retAllow
			if got != c.allow {
				t.Errorf("%s socket(%d, %#x, %d): allowed %v", arch, c.family, c.typ, c.proto, got)
			}
		}
	}
}
