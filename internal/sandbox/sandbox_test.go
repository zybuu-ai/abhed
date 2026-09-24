package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// bash's own /dev/tcp is used so the probe needs no curl inside the sandbox.
func TestProcessSandboxBlocksNetworkByDefault(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	s := processSandbox(t, ws, false)

	out, _ := runIn(t, s, ws,
		"timeout 5 bash -c 'exec 3<>/dev/tcp/93.184.216.34/80' 2>/dev/null && echo REACHED || echo BLOCKED")
	if strings.Contains(out, "REACHED") {
		t.Fatalf("ESCAPE: network reachable with AllowNetwork=false\n%s", out)
	}
	if !strings.Contains(out, "BLOCKED") {
		t.Fatalf("the probe did not run:\n%s", out)
	}
}

// With the network off, the host's interfaces and addresses are not visible
// either: no LAN address or VPN tunnel to learn, as under --unshare-net.
func TestProcessSandboxHidesTheHostsNetwork(t *testing.T) {
	requireNetNS(t)
	ws := workspace(t)
	s := processSandbox(t, ws, false)
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("the host's interfaces cannot be listed: %v", err)
	}
	out, _ := runIn(t, s, ws, "ifconfig -a 2>&1; ip -o addr 2>&1; cat /proc/net/dev 2>&1; netstat -rn 2>&1; route -n get default 2>&1; route -n get 10.0.0.1 2>&1; echo done")
	if !strings.Contains(out, "done") {
		t.Fatalf("the probe did not run:\n%s", out)
	}
	for _, in := range ifaces {
		if in.Flags&net.FlagLoopback != 0 {
			continue
		}
		if regexp.MustCompile(`(^|\s)` + regexp.QuoteMeta(in.Name) + `\b`).MatchString(out) {
			t.Errorf("the host's interface %s is visible with the network off:\n%s", in.Name, out)
		}
		addrs, _ := in.Addrs()
		for _, a := range addrs {
			if ip, _, _ := net.ParseCIDR(a.String()); ip != nil && ip.To4() != nil && strings.Contains(out, ip.String()) {
				t.Errorf("the host's address %s is visible with the network off:\n%s", ip, out)
			}
		}
	}
	for _, leak := range []string{"gateway:", "interface:"} {
		if strings.Contains(out, leak) {
			t.Errorf("the route to the host's network is visible (%s) with the network off:\n%s", leak, out)
		}
	}
	// Ordinary tools still work: a Python that imports socket, git, ls.
	if _, err := exec.LookPath("python3"); err == nil {
		if out, err := runIn(t, s, ws, `python3 -c 'import socket, ssl, uuid; print("py", socket.gethostname() != "")'`); err != nil || !strings.Contains(out, "py True") {
			t.Errorf("python with the network off: %v\n%s", err, out)
		}
	}
	if out, err := runIn(t, s, ws, "git init -q && git status --short && ls -la >/dev/null && echo git-ok"); err != nil || !strings.Contains(out, "git-ok") {
		t.Errorf("git with the network off: %v\n%s", err, out)
	}
}

// A key under the home directory is unreadable from inside the sandbox: the
// Seatbelt profile denies it, and bubblewrap never binds the home directory.
// HOME is pointed at a fresh directory so the test plants nothing real.
func TestProcessSandboxBlocksCredentialRead(t *testing.T) {
	requireNetNS(t)
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	key := filepath.Join(home, ".ssh", "id_probe")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("PRIVATE-KEY-MATERIAL"), 0o600); err != nil {
		t.Fatal(err)
	}

	ws := workspace(t)
	s := processSandbox(t, ws, false)
	out, _ := runIn(t, s, ws, "cat "+key+" 2>&1; echo ---")
	if strings.Contains(out, "PRIVATE-KEY-MATERIAL") {
		t.Fatalf("ESCAPE: a key under the home directory was readable:\n%s", out)
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
