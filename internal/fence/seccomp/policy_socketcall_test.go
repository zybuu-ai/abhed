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
