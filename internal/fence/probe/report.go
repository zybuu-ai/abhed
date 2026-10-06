package probe

import (
	"fmt"
	"strings"
	"time"
)

// MinLandlockABI is the lowest Landlock ABI the fence accepts: ABI 3 is the
// first that controls truncation, so a write grant cannot be widened by it.
const MinLandlockABI = 3

// HelperArg is the first argument with which Run re-executes this binary for
// the checks that confine a process.
const HelperArg = "__abhed_fence_probe"

// Requirements is what the fence about to run needs. The zero value is the
// strictest: network off and a delegated cgroup required.
type Requirements struct {
	// LandlockABI is the lowest Landlock ABI accepted; below MinLandlockABI
	// it is raised to MinLandlockABI.
	LandlockABI int
	// AllowNetwork is true when commands keep the host's network. When false,
	// Landlock must refuse TCP (ABI 4) as a backstop to seccomp.
	AllowNetwork bool
	// CgroupOptional reports a missing delegated cgroup without refusing.
	CgroupOptional bool
	// CgroupDir is the delegated cgroup v2 folder to test; empty means this
	// process's own cgroup.
	CgroupDir string
}

// minABI is the Landlock ABI r asks for, never below the fence's floor.
func (r Requirements) minABI() int { return max(r.LandlockABI, MinLandlockABI) }

// Status is a check's outcome.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
	Skip Status = "skip" // not needed under these requirements
)

// Check IDs, stable for the record.
const (
	CheckPlatform    = "platform"
	CheckNotRoot     = "not_root"
	CheckCaps        = "capabilities"
	CheckNoNewPrivs  = "no_new_privs"
	CheckLandlockABI = "landlock_abi"
	CheckLandlockFS  = "landlock_fs"
	CheckLandlockTCP = "landlock_tcp"
	CheckSeccomp     = "seccomp"
	CheckCgroup      = "cgroup_v2"
	// CheckDelegated is added by the caller that asked the cgroup's manager
	// whether it delegated the cgroup; see Report.Add.
	CheckDelegated = "cgroup_delegated"
)

// Check is one probe and its outcome.
type Check struct {
	ID       string `json:"id"`
	Status   Status `json:"status"`
	Required bool   `json:"required"`
	Reason   string `json:"reason"`
	Value    string `json:"value,omitempty"` // what was measured
}

// Report is the outcome of one Run, shaped for the session record.
type Report struct {
	Qualified   bool      `json:"qualified"`
	Summary     string    `json:"summary"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	Kernel      string    `json:"kernel,omitempty"`  // uname release
	Machine     string    `json:"machine,omitempty"` // uname machine
	LandlockABI int       `json:"landlock_abi"`      // 0 when absent
	Checked     time.Time `json:"checked"`
	Checks      []Check   `json:"checks"`
}

// Check returns the check with id, and whether there is one.
func (r Report) Check(id string) (Check, bool) {
	for _, c := range r.Checks {
		if c.ID == id {
			return c, true
		}
	}
	return Check{}, false
}

// Failures are the required checks that did not pass.
func (r Report) Failures() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Required && c.Status != Pass {
			out = append(out, c)
		}
	}
	return out
}

// Add appends a check the caller made itself, such as asking the cgroup's
// manager, and decides Qualified and the summary again.
func (r *Report) Add(c Check) {
	r.Checks = append(r.Checks, c)
	r.finish()
}

// finish decides Qualified and writes the summary. A report with no checks,
// or a required check that is anything but a pass, is not qualified.
func (r *Report) finish() {
	fails := r.Failures()
	r.Qualified = len(r.Checks) > 0 && len(fails) == 0
	host := strings.TrimSpace(fmt.Sprintf("%s %s %s", r.OS, r.Kernel, r.Arch))
	if r.Qualified {
		passed := 0
		for _, c := range r.Checks {
			if c.Status == Pass {
				passed++
			}
		}
		r.Summary = fmt.Sprintf("qualified for the fence: %s, Landlock ABI %d, %d of %d checks passed", host, r.LandlockABI, passed, len(r.Checks))
		return
	}
	if len(fails) == 0 {
		r.Summary = "not qualified for the fence: nothing was checked on " + host
		return
	}
	parts := make([]string, len(fails))
	for i, c := range fails {
		parts[i] = c.ID + ": " + c.Reason
	}
	r.Summary = fmt.Sprintf("not qualified for the fence on %s: %s", host, strings.Join(parts, "; "))
}

// String is the report for a person: the summary, then a line per check.
func (r Report) String() string {
	var b strings.Builder
	b.WriteString(r.Summary)
	for _, c := range r.Checks {
		req := ""
		if !c.Required && c.Status != Skip {
			req = " (optional)"
		}
		fmt.Fprintf(&b, "\n  %-4s %s%s: %s", c.Status, c.ID, req, c.Reason)
		if c.Value != "" {
			fmt.Fprintf(&b, " [%s]", c.Value)
		}
	}
	return b.String()
}

// landlockKernel names the first upstream Linux release with a Landlock ABI,
// so a refusal can say what would qualify. Distribution backports differ.
func landlockKernel(abi int) string {
	switch abi {
	case 1:
		return "Linux 5.13"
	case 2:
		return "Linux 5.19"
	case 3:
		return "Linux 6.2"
	case 4:
		return "Linux 6.7"
	case 5:
		return "Linux 6.10"
	case 6:
		return "Linux 6.12"
	}
	return "a newer Linux"
}

// abiCheck judges a measured Landlock ABI against the floor wanted.
func abiCheck(id string, have, want int, what string) Check {
	c := Check{ID: id, Required: true, Value: fmt.Sprintf("ABI %d", have)}
	switch {
	case have == 0:
		c.Status, c.Reason = Fail, fmt.Sprintf("Landlock is not available; %s needs Landlock ABI %d (%s or later)", what, want, landlockKernel(want))
	case have < want:
		c.Status, c.Reason = Fail, fmt.Sprintf("Landlock ABI %d is below the %d %s needs (%s or later)", have, want, what, landlockKernel(want))
	default:
		c.Status, c.Reason = Pass, fmt.Sprintf("Landlock ABI %d meets the %d %s needs", have, want, what)
	}
	return c
}
