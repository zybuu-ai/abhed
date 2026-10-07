package sandbox

import "testing"

// clearCaps is a capability-probe output with every set empty, no_new_privs
// set, and the terminator.
const clearCaps = "CapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\n" +
	"CapAmb:\t0000000000000000\nCapBnd:\t0000000000000000\nNoNewPrivs:\t1\n"

func TestCheckCapProbeFailsClosed(t *testing.T) {
	cases := []struct {
		name, out string
		scan, ok  bool
	}{
		{"all clear, no scan required", clearCaps + "PROBE_DONE\n", false, true},
		{"all clear, scan ran", clearCaps + "PROC_SCANNED\nPROBE_DONE\n", true, true},
		{"a capability kept", "CapInh:\t0000000000000000\nCapPrm:\t000001ffffffffff\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\nCapBnd:\t0000000000000000\nNoNewPrivs:\t1\nPROBE_DONE\n", false, false},
		{"bounding set kept", "CapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\nCapBnd:\t000001ffffffffff\nNoNewPrivs:\t1\nPROBE_DONE\n", false, false},
		{"no_new_privs off", "CapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\nCapBnd:\t0000000000000000\nNoNewPrivs:\t0\nPROBE_DONE\n", false, false},
		{"a writable proc file", clearCaps + "WRITABLE /proc/sys/fs/binfmt_misc/register\nPROC_SCANNED\nPROBE_DONE\n", true, false},
		{"a set missing", "CapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\nNoNewPrivs:\t1\nPROBE_DONE\n", false, false},
		{"truncated before the terminator", "CapInh:\t0000000000000000\n", false, false},
		{"scan required but did not run", clearCaps + "PROBE_DONE\n", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why := checkCapProbe(c.out, c.scan)
			if (why == "") != c.ok {
				t.Fatalf("checkCapProbe(scan=%v) ok=%v, got %q", c.scan, c.ok, why)
			}
		})
	}
}
