package probe

import (
	"strings"
	"testing"
)

// Root qualifies only where the managed configuration allows it and Landlock
// scopes signals; an ordinary user always.
func TestRootNeedsSignalScoping(t *testing.T) {
	for _, c := range []struct {
		root, allow bool
		abi         int
		want        Status
		says        string
	}{{false, false, 4, Pass, ""}, {true, true, 5, Fail, "Linux 6.12"}, {true, true, 6, Pass, ""}, {true, true, 7, Pass, ""},
		{true, false, 6, Fail, "fence.allow_root"}, {true, false, 7, Fail, "fence.allow_root"}} {
		got := rootVerdict(Check{ID: CheckNotRoot}, c.root, c.abi, c.allow)
		if got.Status != c.want {
			t.Errorf("root %v allow %v abi %d: %s %s", c.root, c.allow, c.abi, got.Status, got.Reason)
		}
		if c.says != "" && !strings.Contains(got.Reason, c.says) {
			t.Errorf("the refusal does not say what is needed: %s", got.Reason)
		}
	}
}
