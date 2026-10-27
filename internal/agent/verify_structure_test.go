package agent

import (
	"slices"
	"strings"
	"testing"
)

// Headings, labels and a file name alone arrange a reply; they are not
// claims. Sentences beside them still are.
func TestStructureLinesAreNotClaims(t *testing.T) {
	reply := "## Summary\n\n**Summary**\n\nChanges:\n\n- `sum.go`\n- `internal/calc/sum.go`\n- **Files changed**\n\n### Changes made\n\nSummary of changes\n\n1. `sum.go`\n\n**Changes:** `sum.go`\n\nWhat changed\n\n| File | Change |\n|---|---|\n| `sum.go` | added `continue` |\n\nI added `continue` to `Sum`.\n"
	got := splitClaims(reply)
	want := []string{"`sum.go`  ·  added `continue`", "I added `continue` to `Sum`."}
	if !slices.Equal(got, want) {
		t.Fatalf("claims = %q, want %q", got, want)
	}
	for _, s := range []string{"Files changed: 3", "Exit code: `0`", "Tests pass", "`go test ./...` passes", "`sum.go`: added `continue`", "Fixed `sum.go`",
		"Created: `sum_test.go`", "Files changed: `main.go`", "Tests pass: `sum_test.go`", "Modified: `main.go`", "New file: `sum_test.go`"} {
		if _, ok := claimOf(s); !ok {
			t.Errorf("%q was dropped as structure", s)
		}
	}
}

// A test result with no test run behind it says so plainly.
func TestNoTestRunReason(t *testing.T) {
	for _, r := range RunCorpus().Results {
		if r.Name == "a test result said with no test run in the session" || r.Name == "tests pass, only a build ran" {
			if r.Got != ClaimUnverified || !strings.Contains(r.Reason, "no test run in this session") {
				t.Errorf("%s: %s (%s)", r.Name, r.Got, r.Reason)
			}
		}
		// A test run under a runner was one, whether or not it counts.
		if r.Name == "tests pass under uv run --env-file" && strings.Contains(r.Reason, "no test run in this session") {
			t.Errorf("%s: %s", r.Name, r.Reason)
		}
	}
}
