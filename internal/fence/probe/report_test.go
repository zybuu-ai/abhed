package probe

import (
	"encoding/json"
	"strings"
	"testing"
)

func report(checks ...Check) Report {
	r := Report{OS: "linux", Arch: "arm64", Kernel: "6.11.0", LandlockABI: 5, Checks: checks}
	r.finish()
	return r
}

func TestQualifiedOnlyWhenEveryRequiredCheckPasses(t *testing.T) {
	pass := Check{ID: CheckSeccomp, Status: Pass, Required: true, Reason: "ok"}
	skip := Check{ID: CheckLandlockTCP, Status: Skip, Reason: "network on"}
	if r := report(pass, skip); !r.Qualified || !strings.HasPrefix(r.Summary, "qualified for the fence") {
		t.Fatalf("all required passed, got %+v", r)
	}
	fail := Check{ID: CheckLandlockABI, Status: Fail, Required: true, Reason: "Landlock ABI 2 is below the 3 the fence needs (Linux 6.2 or later)"}
	r := report(pass, fail)
	if r.Qualified {
		t.Fatal("a failed required check qualified")
	}
	if !strings.Contains(r.Summary, "landlock_abi: Landlock ABI 2 is below") {
		t.Fatalf("summary does not name the failure: %s", r.Summary)
	}
	if got := r.Failures(); len(got) != 1 || got[0].ID != CheckLandlockABI {
		t.Fatalf("failures = %+v", got)
	}
}

func TestOptionalFailureDoesNotDisqualify(t *testing.T) {
	r := report(Check{ID: CheckSeccomp, Status: Pass, Required: true},
		Check{ID: CheckCgroup, Status: Fail, Required: false, Reason: "not delegated"})
	if !r.Qualified {
		t.Fatalf("an optional failure disqualified: %s", r.Summary)
	}
	if !strings.Contains(r.String(), "cgroup_v2 (optional): not delegated") {
		t.Fatalf("the optional failure is not shown: %s", r.String())
	}
}

func TestRequiredSkipOrEmptyIsNotQualified(t *testing.T) {
	if r := report(); r.Qualified {
		t.Fatal("a report with no checks qualified")
	}
	if r := report(Check{ID: CheckSeccomp, Status: Skip, Required: true}); r.Qualified {
		t.Fatal("a skipped required check qualified")
	}
	if r := report(Check{ID: CheckSeccomp, Status: "", Required: true}); r.Qualified {
		t.Fatal("a required check with no outcome qualified")
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	r := report(Check{ID: CheckNotRoot, Status: Fail, Required: true, Reason: "running as root", Value: "uid 0/0/0"})
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"qualified":false`, `"id":"not_root"`, `"status":"fail"`, `"required":true`, `"value":"uid 0/0/0"`, `"landlock_abi":5`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s: %s", want, b)
		}
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if c, ok := back.Check(CheckNotRoot); !ok || c.Status != Fail || back.Qualified || back.Summary != r.Summary {
		t.Fatalf("round trip lost data: %+v", back)
	}
}

func TestABICheckNamesTheKernel(t *testing.T) {
	cases := []struct {
		have, want int
		status     Status
		says       string
	}{
		{0, 3, Fail, "Landlock is not available; the fence needs Landlock ABI 3 (Linux 6.2 or later)"},
		{2, 3, Fail, "Landlock ABI 2 is below the 3 the fence needs (Linux 6.2 or later)"},
		{3, 4, Fail, "(Linux 6.7 or later)"},
		{5, 3, Pass, "Landlock ABI 5 meets"},
		{6, 9, Fail, "(a newer Linux or later)"},
	}
	for _, tc := range cases {
		c := abiCheck(CheckLandlockABI, tc.have, tc.want, "the fence")
		if c.Status != tc.status || !strings.Contains(c.Reason, tc.says) || !c.Required {
			t.Errorf("abiCheck(%d, %d) = %+v, want %s containing %q", tc.have, tc.want, c, tc.status, tc.says)
		}
	}
}

func TestRequirementsNeverBelowTheFloor(t *testing.T) {
	for in, want := range map[int]int{0: 3, 1: 3, 3: 3, 4: 4, 6: 6} {
		if got := (Requirements{LandlockABI: in}).minABI(); got != want {
			t.Errorf("minABI(%d) = %d, want %d", in, got, want)
		}
	}
}

// A check the caller adds counts as the probe's own do.
func TestAddDecidesAgain(t *testing.T) {
	r := Report{OS: "linux", Checks: []Check{{ID: CheckPlatform, Status: Pass, Required: true}}}
	r.finish()
	if !r.Qualified {
		t.Fatal("one passing check is not qualified")
	}
	r.Add(Check{ID: CheckDelegated, Status: Fail, Required: true, Reason: "systemd says no"})
	if r.Qualified || !strings.Contains(r.Summary, "cgroup_delegated: systemd says no") {
		t.Fatalf("after a failed added check: qualified %v, summary %q", r.Qualified, r.Summary)
	}
}
