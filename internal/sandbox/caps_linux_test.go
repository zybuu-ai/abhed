package sandbox

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// procStatusField reads one line's value from pid's /proc/<pid>/status.
func procStatusField(pid int, key string) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status") //nolint:gosec // a /proc interface file
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// procComm is pid's command name.
func procComm(pid int) string {
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm") //nolint:gosec // a /proc interface file
	return strings.TrimSpace(string(b))
}

// hasAncestor reports whether pid descends from root, following PPid as the
// host sees it: the command runs in a child pid namespace, but from the host
// side its parent chain resolves back to Abhed's own child.
func hasAncestor(pid, root int) bool {
	for i := 0; i < 64 && pid > 1; i++ {
		if pid == root {
			return true
		}
		n, err := strconv.Atoi(procStatusField(pid, "PPid"))
		if err != nil {
			return false
		}
		pid = n
	}
	return pid == root
}

// sandboxedStatus starts a sleep under the sandbox and reads that command's
// capability lines from the host side, so the assertion needs no fresh /proc
// inside the sandbox.
func sandboxedStatus(t *testing.T, s *Process, ws string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// exec replaces bash with sleep, so the command is bwrap's own descendant.
	cmd := s.Command(ctx, ws, "exec sleep 300")
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })

	pid := 0
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && pid == 0; {
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			p, err := strconv.Atoi(e.Name())
			if err == nil && procComm(p) == "sleep" && hasAncestor(p, cmd.Process.Pid) {
				pid = p
				break
			}
		}
		if pid == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if pid == 0 {
		t.Fatalf("no sleep descendant of %d appeared", cmd.Process.Pid)
	}

	got := map[string]string{}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status") //nolint:gosec // a /proc interface file
	if err != nil {
		t.Fatalf("reading the command's status: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && (strings.HasPrefix(k, "Cap") || k == "NoNewPrivs") {
			got[k] = strings.TrimSpace(v)
		}
	}
	return got
}

// assertNoCapabilities fails unless the command held no capability it could
// use and could gain none: the bounding set is emptied too, so no setuid or
// file-capability binary can refill it.
func assertNoCapabilities(t *testing.T, got map[string]string) {
	t.Helper()
	for _, k := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb", "CapBnd"} {
		v, ok := got[k]
		n, err := strconv.ParseUint(v, 16, 64)
		if !ok || err != nil || n != 0 {
			t.Errorf("%s is %q in the sandboxed command (uid %d); want 0", k, v, os.Getuid())
		}
	}
	if got["NoNewPrivs"] != "1" {
		t.Errorf("NoNewPrivs is %q in the sandboxed command; want 1", got["NoNewPrivs"])
	}
}

// bwrapProcess is a process sandbox on bubblewrap, or a skip.
func bwrapProcess(t *testing.T, ws string) *Process {
	t.Helper()
	p := DefaultPolicy(ws)
	p.AllowNetwork = true
	s := NewProcess(p)
	if s.Backend() != "bwrap" {
		t.Skipf("the process sandbox here is not bubblewrap: %q", s.Backend())
	}
	ok, why := s.Available()
	if !ok {
		// As root with no private /proc the tier refuses the host-/dev
		// fallback; that refusal is the fix working, so the test is done.
		if rootCaps() && strings.Contains(why, "block devices") {
			t.Skipf("root refuses the process tier without a private /proc: %s", why)
		}
		t.Skipf("process sandbox unavailable: %s", why)
	}
	return s
}

// A sandboxed command holds no capabilities and cannot gain any, whoever runs
// Abhed. As root, bwrap kept every one and the command stayed uid 0.
func TestProcessSandboxCommandHoldsNoCapabilities(t *testing.T) {
	ws := workspace(t)
	s := bwrapProcess(t, ws)
	assertNoCapabilities(t, sandboxedStatus(t, s, ws))
}

// A sandboxed command cannot write the root-owned /proc files that would run
// code as host root. core_pattern and modprobe are the escapes and are checked
// whatever the uid (off root the wrong owner blocks them, as root the covers
// do); the other covered files are checked as root, where they are bound.
func TestProcessSandboxProcEscapesClosed(t *testing.T) {
	ws := workspace(t)
	s := bwrapProcess(t, ws)
	files := []string{"/proc/sys/kernel/core_pattern", "/proc/sys/kernel/modprobe"}
	if rootCaps() {
		files = append(files, "/proc/dynamic_debug/control", "/proc/latency_stats", "/proc/pressure/cpu")
	}
	check := "for f in " + strings.Join(files, " ") + `; do test -w "$f" && echo "WRITABLE $f"; done; echo OK`
	out, err := runIn(t, s, ws, check)
	if err != nil {
		t.Fatalf("probe command: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Fatalf("probe command did not finish:\n%s", out)
	}
	if strings.Contains(out, "WRITABLE") {
		t.Fatalf("a sandboxed command can still write a host-root /proc file:\n%s", out)
	}
}
