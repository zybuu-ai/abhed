package seccomp

import (
	"slices"
	"testing"
)

func TestRefusableCalls(t *testing.T) {
	got := RefusableCalls()
	for _, want := range []string{"ptrace", "mount", "socket", "socketcall", "bpf", "kill", "clone3"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s missing from %v", want, got)
		}
	}
	if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Fatal("not sorted and unique")
	}
}
