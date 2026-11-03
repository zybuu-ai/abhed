package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordVerifyQuietHelper runs abhed record with the argument a test below passes.
func TestRecordVerifyQuietHelper(t *testing.T) {
	sub := os.Getenv("ABHED_RECORD_QUIET_SUB")
	if sub == "" {
		t.Skip("run by TestRecordVerifyHasNoConfigWarnings")
	}
	os.Exit(Main([]string{"record", sub}))
}

// Verify keeps the warnings about where the record is, how it is stored and
// witnessed, and drops those about session settings, which bury its answer.
func TestRecordVerifyHasNoConfigWarnings(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	// An allowlist without allow_network is set aside, with a warning per key;
	// the mistyped witness and storage keys change what verify checks.
	cfg := `{"sandbox":{"network":"allowlist"},"egress":{"default":"deny","rules":[{"host":"pypi.org","decision":"allow"}]},` +
		`"record":{"witnes":{"file":"/x"}},"storage":{"drivr":"postgres"}}`
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(sub string) string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRecordVerifyQuietHelper$")
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "ABHED_RECORD_QUIET_SUB="+sub, "HOME="+home)
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	if out := run("list"); !strings.Contains(out, "which is ignored") {
		t.Fatalf("list should still warn of the set-aside settings:\n%s", out)
	}
	out := run("verify")
	if strings.Contains(out, "egress.") || strings.Contains(out, "sandbox.network") {
		t.Fatalf("verify printed warnings about session settings:\n%s", out)
	}
	if !strings.Contains(out, "record.witnes") || !strings.Contains(out, "storage.drivr") {
		t.Fatalf("verify dropped the warnings about the record's own settings:\n%s", out)
	}
	if !strings.Contains(out, "session(s) checked") {
		t.Fatalf("verify did not run:\n%s", out)
	}
}
