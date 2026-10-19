package seccomp

import (
	"slices"
	"sync"
)

// RefusableCalls are the system calls the profile can refuse on any
// architecture or network setting, sorted: the names a refusal is counted by.
var RefusableCalls = sync.OnceValue(func() []string {
	var out []string
	for _, info := range arches {
		for _, p := range []Policy{{}, {AllowNetwork: true}, {AllowNetwork: true, ProxyOnly: true}} {
			p.Abhed = 2
			for _, r := range p.rules(info) {
				out = append(out, r.name)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
})
