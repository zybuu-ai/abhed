package sandbox

import "testing"

// full is a capability-probe output with every set empty, no_new_privs set,
// no writable /proc file, and the terminator.
const full = "CapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\n" +
	"CapAmb:\t0000000000000000\nCapBnd:\t0000000000000000\nNoNewPrivs:\t1\nPROBE_DONE\n"

func TestCheckCapProbeFailsClosed(t *testing.T) {
	cases := []struct {
		name, out string
		ok        bool
	}{
		{"all clear", full, true},
		{"a capability kept", "CapInh:\t0000000000000000\nCapPrm:\t000001ffffffffff\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\nCapBnd:\t0000000000000000\nNoNewPrivs:\t1\nPROBE_DONE\n", false},
		{"bounding set kept", "CapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\nCapBnd:\t000001ffffffffff\nNoNewPrivs:\t1\nPROBE_DONE\n", false},
		{"no_new_privs off", "CapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\nCapBnd:\t0000000000000000\nNoNewPrivs:\t0\nPROBE_DONE\n", false},
		{"a writable proc file", full + "WRITABLE /proc/sys/kernel/core_pattern\n", false},
		{"a set missing", "CapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\nNoNewPrivs:\t1\nPROBE_DONE\n", false},
		{"truncated before the terminator", "CapInh:\t0000000000000000\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why := checkCapProbe(c.out)
			if (why == "") != c.ok {
				t.Fatalf("checkCapProbe ok=%v, got %q", c.ok, why)
			}
		})
	}
}
