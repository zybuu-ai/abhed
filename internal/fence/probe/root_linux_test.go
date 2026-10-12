package probe

import (
	"strings"
	"testing"
)

// Root qualifies only where Landlock scopes signals; an ordinary user always.
func TestRootNeedsSignalScoping(t *testing.T) {
	for _, c := range []struct {
		root bool
		abi  int
		want Status
	}{{false, 4, Pass}, {true, 5, Fail}, {true, 6, Pass}, {true, 7, Pass}} {
		got := rootVerdict(Check{ID: CheckNotRoot}, c.root, c.abi)
		if got.Status != c.want {
			t.Errorf("root %v abi %d: %s %s", c.root, c.abi, got.Status, got.Reason)
		}
		if c.root && c.want == Fail && !strings.Contains(got.Reason, "Linux 6.12") {
			t.Errorf("the refusal does not say what is needed: %s", got.Reason)
		}
	}
}
