//go:build !windows

package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
)

// Instances are kept apart in temp space as in their workspaces: each has a
// TMPDIR of its own, the shared temp areas are out of reach, and bob's shell
// cannot read what alice's command left in hers.
func TestRealSandboxesKeepTempApart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the host's sandbox")
	}
	// The process tier, not a container engine the host may also have.
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	ctx := context.Background()
	d := New(config.Default())
	for _, id := range []string{"alice", "bob"} {
		st, err := d.Create(ctx, driver.Spec{ID: id, Workspace: localDir(t), MinTier: "process"})
		if errors.Is(err, driver.ErrUnavailable) {
			if _, statErr := os.Stat("/usr/bin/sandbox-exec"); statErr == nil {
				t.Fatalf("Seatbelt is present but the driver refused: %v", err)
			}
			t.Skipf("no process tier on this host: %v", err)
		}
		if err != nil {
			t.Fatal(err)
		}
		if st.Tier != "process" {
			t.Skipf("the sandbox here is %s, not the process tier", st.Tier)
		}
		t.Cleanup(func() { _ = d.Delete(ctx, id) })
	}
	shared := "abhedtest-alice-" + filepath.Base(t.TempDir())
	for _, dir := range []string{"/tmp", "/var/tmp"} {
		t.Cleanup(func() { _ = os.Remove(filepath.Join(dir, shared)) })
	}
	var out syncBuffer
	res, err := d.Exec(ctx, "alice", driver.ExecRequest{Stdout: &out, Command: `echo alice-secret > "$TMPDIR/note"; cat "$TMPDIR/note"; echo "tmp=$TMPDIR"; ` +
		`echo alice-secret > /tmp/` + shared + ` || echo tmp-refused; echo alice-secret > /var/tmp/` + shared + ` || echo vartmp-refused`})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("alice: %v %v %s", res, err, out.String())
	}
	got := out.String()
	_, aliceTmp, _ := strings.Cut(got, "tmp=")
	aliceTmp = strings.TrimSpace(strings.SplitN(aliceTmp, "\n", 2)[0])
	if !strings.Contains(got, "alice-secret") || aliceTmp == "" || !strings.Contains(got, "tmp-refused") || !strings.Contains(got, "vartmp-refused") {
		t.Fatalf("alice's own temp, or the shared areas' refusal:\n%s", got)
	}
	var bob syncBuffer
	_, err = driver.Attach(ctx, d, "bob", driver.AttachRequest{
		Stdin: strings.NewReader(`cat "` + aliceTmp + `/note"; ls "` + aliceTmp + `"; cat /tmp/` + shared + `; cat /var/tmp/` + shared +
			`; ls /tmp /var/tmp "$(dirname "$TMPDIR")"; echo bob-own > "$TMPDIR/b" && cat "$TMPDIR/b"; echo "bobtmp=$TMPDIR"` + "\nexit\n"),
		Stdout: &bob, Size: driver.WinSize{Rows: 24, Cols: 200},
	})
	if err != nil {
		t.Fatalf("bob's shell: %v %s", err, bob.String())
	}
	if strings.Contains(bob.String(), "alice-secret") || strings.Contains(bob.String(), "note\r\n") {
		t.Fatalf("bob's shell read alice's temp:\n%s", bob.String())
	}
	// Under bubblewrap each sandbox's own folder is bound at /tmp, so the paths
	// match there; that bob cannot read alice's note above is what keeps them apart.
	m := regexp.MustCompile(`bobtmp=(/\S+)`).FindStringSubmatch(bob.String())
	if !strings.Contains(bob.String(), "bob-own") || m == nil || m[1] == aliceTmp && aliceTmp != "/tmp" {
		t.Fatalf("bob's own temp:\n%s", bob.String())
	}
}
