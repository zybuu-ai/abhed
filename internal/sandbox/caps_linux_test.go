package sandbox

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// capStatus reads the Cap* and NoNewPrivs lines a sandboxed process printed
// from its /proc/self/status.
func capStatus(t *testing.T, out string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && (strings.HasPrefix(k, "Cap") || k == "NoNewPrivs") {
			got[k] = strings.TrimSpace(v)
		}
	}
	if len(got) == 0 {
		t.Fatalf("no capability lines in the command's output:\n%s", out)
	}
	return got
}

// assertNoCapabilities fails unless the process held no capability it could
// use, and could gain none: root's bounding set is emptied too, since under
// root a setuid or file-capability binary would otherwise refill it.
func assertNoCapabilities(t *testing.T, got map[string]string) {
	t.Helper()
	sets := []string{"CapInh", "CapPrm", "CapEff", "CapAmb"}
	if os.Getuid() == 0 {
		sets = append(sets, "CapBnd")
	}
	for _, k := range sets {
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

// A command and a terminal shell under bubblewrap hold no capabilities,
// whoever runs Abhed. As root, bwrap kept every one unless told to drop them.
func TestProcessSandboxCommandHoldsNoCapabilities(t *testing.T) {
	ws := workspace(t)
	s := processSandbox(t, ws, true).(*Process)
	if !s.bwrapFreshOK() {
		t.Skip("bwrap cannot mount a fresh /proc here, so the command cannot read its own status")
	}
	out, err := runIn(t, s, ws, "cat /proc/self/status")
	if err != nil {
		t.Fatalf("command: %v\n%s", err, out)
	}
	assertNoCapabilities(t, capStatus(t, out))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sh := s.Shell(ctx, ws)
	sh.Stdin = strings.NewReader("cat /proc/self/status\nexit\n")
	shOut, err := sh.CombinedOutput()
	if err != nil {
		t.Fatalf("shell: %v\n%s", err, shOut)
	}
	assertNoCapabilities(t, capStatus(t, string(shOut)))
}
