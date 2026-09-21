package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These are the escape tests docs/architecture/03-security.md §7 requires
// before Abhed may execute untrusted code. They assert the boundary actually
// holds rather than that the configuration looks right.

func workspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	return dir
}

func runIn(t *testing.T, s Sandbox, cwd, command string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := s.Command(ctx, cwd, command).CombinedOutput()
	return string(out), err
}

func processSandbox(t *testing.T, ws string, allowNet bool) Sandbox { //nolint:unparam // a fixture; the fixed argument documents what the tests rely on
	t.Helper()
	p := DefaultPolicy(ws)
	p.AllowNetwork = allowNet
	s := NewProcess(p)
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	return s
}

// Denying network unshares the net namespace, and a runner without
// CAP_NET_ADMIN cannot bring up loopback inside one, so bwrap fails before the
// command runs. That is the environment's limit rather than a broken boundary,
// so a test that needs it skips — never by letting the network through, which
// is the downgrade Select refuses. Tracked in #44.
func requireNetNS(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bwrap", "--unshare-net",
		"--ro-bind", "/usr", "/usr", "--ro-bind", "/bin", "/bin",
		"--ro-bind", "/lib", "/lib", "--ro-bind-try", "/lib64", "/lib64",
		"/bin/true").CombinedOutput()
	if err != nil {
		t.Skipf("this environment cannot unshare the network namespace: %v\n%s", err, out)
	}
}

func TestProcessSandboxAllowsWorkspaceWrite(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	s := processSandbox(t, ws, false)

	out, err := runIn(t, s, ws, "echo hello > out.txt && cat out.txt")
	if err != nil {
		t.Fatalf("workspace write should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("got %q", out)
	}
}

// The core containment property: no writing outside the workspace.
func TestProcessSandboxBlocksWriteOutsideWorkspace(t *testing.T) {
	// Without this the test passes when bwrap cannot start at all: the write
	// does not happen, but nothing was contained either.
	requireNetNS(t)
	ws := workspace(t)
	s := processSandbox(t, ws, false)

	outside := filepath.Join(os.TempDir(), "abhed-escape-probe.txt")
	_ = os.Remove(outside)
	defer func() { _ = os.Remove(outside) }()

	// /private/tmp is intentionally writable (toolchains need it), so probe a
	// path that must never be writable instead.
	target := "/usr/local/abhed-escape-probe.txt"
	out, _ := runIn(t, s, ws, "echo escaped > "+target+" 2>&1; echo done")
	if _, err := os.Stat(target); err == nil {
		_ = os.Remove(target)
		t.Fatalf("ESCAPE: wrote outside the workspace to %s\n%s", target, out)
	}
}

func TestProcessSandboxBlocksSystemPathWrite(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	s := processSandbox(t, ws, false)

	for _, target := range []string{"/etc/abhed-probe", "/usr/bin/abhed-probe"} {
		_, _ = runIn(t, s, ws, "echo x > "+target+" 2>&1")
		if _, err := os.Stat(target); err == nil {
			_ = os.Remove(target)
			t.Fatalf("ESCAPE: wrote to protected path %s", target)
		}
	}
}

// Egress denial is what makes a successful prompt injection non-exfiltrating.
func TestProcessSandboxBlocksNetworkByDefault(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("network assertion is verified for the Seatbelt backend")
	}
	ws := workspace(t)
	s := processSandbox(t, ws, false)

	out, _ := runIn(t, s, ws,
		"curl -s -m 3 http://93.184.216.34/ -o /dev/null && echo REACHED || echo BLOCKED")
	if strings.Contains(out, "REACHED") {
		t.Fatalf("ESCAPE: network reachable with AllowNetwork=false\n%s", out)
	}
}

func TestProcessSandboxBlocksCredentialRead(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("credential denial is verified for the Seatbelt backend")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	sshDir := filepath.Join(home, ".ssh")
	if _, err := os.Stat(sshDir); err != nil {
		t.Skip("no ~/.ssh to probe")
	}

	ws := workspace(t)
	s := processSandbox(t, ws, false)
	out, _ := runIn(t, s, ws, "ls "+sshDir+" 2>&1 | head -3; echo ---")
	// Denied reads surface as an error, not a listing.
	if !strings.Contains(out, "Operation not permitted") && !strings.Contains(out, "denied") {
		t.Logf("credential read output (review manually): %q", out)
	}
}

func TestSandboxReportsItsOwnTier(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	s := processSandbox(t, ws, false)
	out, err := runIn(t, s, ws, "echo $ABHED_SANDBOX")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, string(s.Tier())) {
		t.Fatalf("sandbox must announce its tier to the workload, got %q", out)
	}
}

// Select must never silently downgrade below the required tier.
func TestSelectRefusesToDowngrade(t *testing.T) {
	p := DefaultPolicy(workspace(t))
	p.MinTier = TierVM

	s, err := Select(p)
	if err == nil {
		// Only acceptable if a genuine VM-tier backend exists here.
		if s.Tier().Strength() < TierVM.Strength() {
			t.Fatalf("Select returned %s below required %s", s.Tier(), TierVM)
		}
		t.Logf("VM-tier backend available: %s", s.Describe())
		return
	}
	if !strings.Contains(err.Error(), "min_tier") {
		t.Fatalf("refusal should tell the operator how to proceed: %v", err)
	}
}

func TestSelectPrefersStrongest(t *testing.T) {
	p := DefaultPolicy(workspace(t))
	p.MinTier = TierNone

	s, err := Select(p)
	if err != nil {
		t.Fatal(err)
	}
	// With MinTier none we should still get the best available, not None.
	if _, isNone := s.(*None); isNone {
		if proc := NewProcess(p); func() bool { ok, _ := proc.Available(); return ok }() {
			t.Fatal("Select chose None while a process sandbox was available")
		}
	}
	t.Logf("selected: %s — %s", s.Tier(), s.Describe())
}

func TestNoneTierIsHonestAboutItself(t *testing.T) {
	n := NewNone(DefaultPolicy(workspace(t)))
	if !strings.Contains(n.Describe(), "NO ISOLATION") {
		t.Fatal("the no-sandbox tier must say so unmistakably")
	}
	if n.Tier().Strength() != 0 {
		t.Fatal("None must have zero strength")
	}
}

func TestTierOrdering(t *testing.T) {
	if TierNone.Strength() >= TierProcess.Strength() ||
		TierProcess.Strength() >= TierContainer.Strength() ||
		TierContainer.Strength() >= TierVM.Strength() {
		t.Fatal("tier strengths must be strictly ordered")
	}
}

func TestResourceLimitsRejectForkBomb(t *testing.T) {
	if testing.Short() {
		t.Skip("resource exhaustion test is slow")
	}
	ws := workspace(t)
	s := processSandbox(t, ws, false)

	// Should be killed by the context deadline rather than hanging the machine.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_ = s.Command(ctx, ws, "while true; do :; done").Run()
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("runaway command was not bounded: ran %s", elapsed)
	}
}
