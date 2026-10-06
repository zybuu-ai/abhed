//go:build linux

package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// requireFence skips unless ABHED_REQUIRE_FENCE is set, as on a host meant to
// qualify; there every check is expected to run for real.
func requireFence(t *testing.T) {
	t.Helper()
	if os.Getenv("ABHED_REQUIRE_FENCE") == "" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 on a host that should qualify for the fence")
	}
}

func logReport(t *testing.T, r Report) {
	t.Helper()
	b, _ := json.MarshalIndent(r, "", "  ")
	t.Logf("\n%s\n%s", r, b)
}

// On a qualified host every check passes, unless this is root, which the
// fence refuses in this release.
func TestQualifiedHost(t *testing.T) {
	requireFence(t)
	r := Run(context.Background(), Requirements{})
	logReport(t, r)
	if os.Geteuid() == 0 {
		c, _ := r.Check(CheckNotRoot)
		if r.Qualified || c.Status != Fail || !strings.Contains(r.Summary, "refuses root") {
			t.Fatalf("root was not refused: %s", r.Summary)
		}
		return
	}
	if !r.Qualified {
		t.Fatalf("not qualified: %s", r.Summary)
	}
	for _, c := range r.Checks {
		if c.Status != Pass {
			t.Errorf("%s: %s (%s)", c.ID, c.Status, c.Reason)
		}
	}
	if r.Kernel == "" || r.Machine == "" || r.LandlockABI < MinLandlockABI {
		t.Errorf("kernel %q, machine %q, ABI %d not recorded", r.Kernel, r.Machine, r.LandlockABI)
	}
}

// Asking for a Landlock ABI above the host's refuses, and says why.
func TestHigherLandlockABIIsRefused(t *testing.T) {
	requireFence(t)
	abi, err := landlockABI()
	if err != nil {
		t.Fatal(err)
	}
	r := Run(context.Background(), Requirements{LandlockABI: abi + 1})
	logReport(t, r)
	c, _ := r.Check(CheckLandlockABI)
	want := fmt.Sprintf("Landlock ABI %d is below the %d", abi, abi+1)
	if r.Qualified || c.Status != Fail || !strings.Contains(c.Reason, want) || !strings.Contains(r.Summary, want) {
		t.Fatalf("want a refusal naming %q, got %s", want, r.Summary)
	}
}

// With the network on, the TCP refusal is not needed; a cgroup that is not
// delegated is reported but, when optional, does not refuse.
func TestRequirementsShapeTheChecks(t *testing.T) {
	requireFence(t)
	r := Run(context.Background(), Requirements{AllowNetwork: true, CgroupOptional: true, CgroupDir: "/sys/fs/cgroup/../../tmp"})
	logReport(t, r)
	if c, _ := r.Check(CheckLandlockTCP); c.Status != Skip || c.Required {
		t.Errorf("tcp with the network on: %+v", c)
	}
	if c, _ := r.Check(CheckCgroup); c.Status != Fail || c.Required || !strings.Contains(c.Reason, "not under /sys/fs/cgroup") {
		t.Errorf("cgroup outside the hierarchy: %+v", c)
	}
	if os.Geteuid() != 0 && !r.Qualified {
		t.Errorf("an optional cgroup failure refused: %s", r.Summary)
	}
}

// The mounts check is made either way, required only when asked for, and
// leaves the folder it tested in removed.
func TestMountsCheckIsRequiredOnlyWhenAsked(t *testing.T) {
	requireFence(t)
	for _, want := range []bool{false, true} {
		r := Run(context.Background(), Requirements{Mounts: want})
		c, ok := r.Check(CheckMounts)
		if !ok || c.Required != want || c.Status == Skip {
			t.Fatalf("mounts %v: %+v", want, c)
		}
		if want && c.Status != Pass && r.Qualified {
			t.Errorf("a required mounts failure qualified: %s", r.Summary)
		}
		t.Logf("mounts %v: %s %s", want, c.Status, c.Reason)
	}
	left, _ := filepath.Glob(filepath.Join(os.TempDir(), "abhed-fence-probe-mounts-*"))
	if len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// Run confines only its helpers: the caller keeps no_new_privs off, its
// seccomp mode and its filesystem.
func TestRunLeavesTheCallerUnconfined(t *testing.T) {
	before, _ := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0)
	_ = Run(context.Background(), Requirements{})
	if v, _ := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0); v != 0 {
		t.Error("Run set no_new_privs on its caller")
	}
	if after, _ := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0); after != before {
		t.Errorf("Run changed the caller's seccomp mode from %d to %d", before, after)
	}
	if err := os.WriteFile(filepath.Join(t.TempDir(), "x"), []byte("x"), 0o600); err != nil {
		t.Errorf("the caller cannot write after Run: %v", err)
	}
	if _, err := os.ReadFile("/proc/self/status"); err != nil {
		t.Errorf("the caller cannot read after Run: %v", err)
	}
}

// A helper that cannot answer is a failure, never a pass.
func TestUnknownHelperStageFails(t *testing.T) {
	if _, err := runHelper(context.Background(), "no-such-stage"); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("got %v", err)
	}
}
