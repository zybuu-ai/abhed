package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// claimFiles are where the process tier's mechanism is described to readers.
var claimFiles = []string{
	"sandbox.go",
	"process.go",
	"../../docs/trust/security-posture.md",
	"../../docs/architecture/03-security.md",
	"../../docs/guide/02-configuration.md",
	"../../README.md",
	"../../USAGE.md",
}

// A syscall filter named in the docs must be one the bwrap command applies:
// the claim was once made with none in place.
func TestDocsClaimNoSyscallFilterTheSandboxLacks(t *testing.T) {
	b := &Process{policy: DefaultPolicy(workspace(t)), backend: "bwrap"}
	args := b.wrap(t.Context(), b.policy.Workspace, nil, "/bin/true").Args
	for _, a := range args {
		if a == "--seccomp" || a == "--add-seccomp-fd" {
			t.Skip("bwrap now applies a seccomp filter; revisit this test and the docs together")
		}
	}
	mention := regexp.MustCompile(`(?i)seccomp|landlock`)
	negated := regexp.MustCompile(`(?i)\bno\b|\bnot\b|without|filters no`)
	for _, f := range claimFiles {
		data, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if mention.MatchString(line) && !negated.MatchString(line) {
				t.Errorf("%s:%d claims a filter bwrap is not given: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
