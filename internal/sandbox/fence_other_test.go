//go:build !linux

package sandbox

import (
	"runtime"
	"strings"
	"testing"
)

// Where the probe does not qualify the host, Select refuses and names the
// failing check; off Linux it never qualifies. The Linux tests cover the
// refusal there.
func TestSelectRefusesTheFenceNamingTheFailingCheck(t *testing.T) {
	_, err := Select(fencePolicy(t))
	if err == nil || !strings.Contains(err.Error(), "platform:") || !strings.Contains(err.Error(), "needs Linux") {
		t.Fatalf("fence on %s: %v", runtime.GOOS, err)
	}
}
