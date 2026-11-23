package secrets

import (
	"fmt"
	"maps"
	"sync"
)

// extra are values Abhed holds outside any store, such as a model key read
// from the Keychain or the environment: every redactor built in this process
// redacts them as it does stored values, so the key reaches neither the
// model nor the record.
var extra struct {
	sync.Mutex
	m   map[string]string
	gen uint64
}

// AlsoRedact has every redactor in this process replace value with
// [secret:name] from now on. A value shorter than MinLength is ignored, as it
// would match ordinary text.
func AlsoRedact(name, value string) {
	if len(value) < MinLength {
		return
	}
	extra.Lock()
	defer extra.Unlock()
	if extra.m == nil {
		extra.m = map[string]string{}
	}
	for _, v := range extra.m {
		if v == value {
			return
		}
	}
	label := name
	for i := 2; ; i++ {
		if _, taken := extra.m[label]; !taken {
			break
		}
		label = fmt.Sprintf("%s_%d", name, i)
	}
	extra.m[label] = value
	extra.gen++
}

// extraGen changes whenever AlsoRedact adds a value, so a Fresh redactor
// knows to rebuild.
func extraGen() uint64 {
	extra.Lock()
	defer extra.Unlock()
	return extra.gen
}

// withExtra is m with each extra value m does not already hold.
func withExtra(m map[string]string) map[string]string {
	extra.Lock()
	defer extra.Unlock()
	if len(extra.m) == 0 {
		return m
	}
	have := map[string]bool{}
	for _, v := range m {
		have[v] = true
	}
	out := maps.Clone(m)
	if out == nil {
		out = map[string]string{}
	}
	for n, v := range extra.m {
		if have[v] {
			continue
		}
		if _, taken := out[n]; taken {
			n = "MODEL_KEY_" + n
		}
		out[n] = v
	}
	return out
}
