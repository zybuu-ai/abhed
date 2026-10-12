package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// abhed trust startup-files shows the start-up files, records them once the
// person says so, and records nothing otherwise; doctor then lists them and
// any changed since.
func TestTrustStartupFilesRecordsTheBaseline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	prof := filepath.Join(home, ".zprofile")
	if err := os.WriteFile(prof, []byte("eval \"$(/opt/homebrew/bin/brew shellenv)\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := trustIn
	t.Cleanup(func() { trustIn = old })
	trustIn = strings.NewReader("n\n")
	var out bytes.Buffer
	if code := trustCmd(t.TempDir(), []string{"startup-files"}, &out); code != 1 || !strings.Contains(out.String(), "brew shellenv") {
		t.Fatalf("declined: %d %s", code, out.String())
	}
	if files, _ := config.TrustedStartupFiles(); len(files) != 0 {
		t.Fatalf("recorded without a yes: %+v", files)
	}
	trustIn = strings.NewReader("y\n")
	out.Reset()
	if code := trustCmd(t.TempDir(), []string{"startup-files"}, &out); code != 0 {
		t.Fatalf("accepted: %d %s", code, out.String())
	}
	base := config.StartupBaseline()
	if f, ok := base[prof]; !ok || f.Size == 0 || len(f.SHA256) != 64 {
		t.Fatalf("baseline: %+v", base)
	}
	out.Reset()
	printDoctorStartupFiles(&out)
	if !strings.Contains(out.String(), "trusted (sha256") {
		t.Fatalf("doctor: %s", out.String())
	}
	if err := os.WriteFile(prof, []byte("curl x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	printDoctorStartupFiles(&out)
	if !strings.Contains(out.String(), "changed since you trusted it") || !strings.Contains(out.String(), "abhed trust startup-files") {
		t.Fatalf("doctor after a change: %s", out.String())
	}
	// -y records without asking, as a script would.
	trustIn = strings.NewReader("")
	if code := trustCmd(t.TempDir(), []string{"startup-files", "-y"}, &out); code != 0 {
		t.Fatalf("-y: %d", code)
	}
	if selfAdmin([]string{"trust", "startup-files"}) == "" {
		t.Fatal("not refused inside an agent's command")
	}
}
