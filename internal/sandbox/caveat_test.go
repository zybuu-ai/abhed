package sandbox

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// Where bubblewrap cannot mount a fresh /proc, commands run without one;
// the sandbox says so rather than reporting a plain process tier.
func TestProcessCaveatNamesAMissingProc(t *testing.T) {
	old := rootCaps
	rootCaps = func() bool { return false }
	t.Cleanup(func() { rootCaps = old })

	noProc := &Process{backend: "bwrap"}
	noProc.freshOnce.Do(func() {})
	if got := Caveat(noProc); got != "no /proc" {
		t.Fatalf("caveat %q, want no /proc", got)
	}
	if d := noProc.Describe(); !strings.Contains(d, "no /proc") {
		t.Fatalf("Describe does not say /proc is missing: %s", d)
	}
	withProc := &Process{backend: "bwrap"}
	withProc.freshOnce.Do(func() { withProc.freshOK = true })
	if got := Caveat(withProc); got != "" || strings.Contains(withProc.Describe(), "/proc") {
		t.Fatalf("a sandbox with /proc has caveat %q: %s", got, withProc.Describe())
	}
	if got := Caveat(&Process{backend: "sandbox-exec"}); got != "" {
		t.Fatalf("seatbelt caveat %q", got)
	}
	if got := Caveat(&None{}); got != "" {
		t.Fatalf("a sandbox without caveats has %q", got)
	}
}

// The Localhost seccomp profile the chart ships is the engine default plus
// the namespace and mount calls bubblewrap needs, on every architecture the
// image is built for, and denies the rest.
func TestShippedBubblewrapSeccompProfile(t *testing.T) {
	data, err := os.ReadFile("../../deploy/helm/abhed/files/seccomp-bwrap.json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		DefaultAction string `json:"defaultAction"`
		ArchMap       []struct {
			Architecture string `json:"architecture"`
		} `json:"archMap"`
		Syscalls []struct {
			Names    []string        `json:"names"`
			Action   string          `json:"action"`
			Args     json.RawMessage `json:"args"`
			Includes json.RawMessage `json:"includes"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if p.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Fatalf("default action %s", p.DefaultAction)
	}
	var arches []string
	for _, a := range p.ArchMap {
		arches = append(arches, a.Architecture)
	}
	for _, want := range []string{"SCMP_ARCH_X86_64", "SCMP_ARCH_AARCH64", "SCMP_ARCH_S390X"} {
		if !slices.Contains(arches, want) {
			t.Errorf("no %s in %v", want, arches)
		}
	}
	allowed := map[string]bool{}
	for _, s := range p.Syscalls {
		// A rule for some capabilities or architectures only is not unconditional.
		if s.Action == "SCMP_ACT_ALLOW" && len(s.Args) == 0 && len(s.Includes) == 0 {
			for _, n := range s.Names {
				allowed[n] = true
			}
		}
	}
	for _, n := range []string{"unshare", "clone", "setns", "mount", "umount2", "pivot_root"} {
		if !allowed[n] {
			t.Errorf("%s is not allowed", n)
		}
	}
	for _, n := range []string{"keyctl", "add_key", "bpf", "kexec_load", "open_by_handle_at", "init_module"} {
		if allowed[n] {
			t.Errorf("%s is allowed unconditionally", n)
		}
	}
}
