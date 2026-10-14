package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// A run records the start-up files left unjudged by the person's baseline,
// once per loop, before its first message.
func TestRunRecordsTheTrustedStartupFiles(t *testing.T) {
	home := t.TempDir()
	prof := filepath.Join(home, ".zprofile")
	if err := os.WriteFile(prof, []byte("export A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	size, sum, _ := policy.StartupFileMark(prof)
	l, store := harnessIn(t, tempDir(t), nil, policy.ModeBypass, true)
	l.Policy.Home = func() (string, bool) { return home, true }
	l.Policy.StartupBaseline = map[string]policy.StartupFile{prof: {Size: size, SHA256: sum}}
	for range 2 {
		if _, err := l.Run(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := store.Events("sess1")
	var noted []policy.TrustedStartupFile
	n := 0
	for _, e := range evs {
		if e.Type == EvStartupTrusted {
			n++
			var p struct {
				Files []policy.TrustedStartupFile `json:"files"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			noted = p.Files
		}
	}
	if n != 1 || len(noted) != 1 || noted[0].Path != prof || noted[0].SHA256 != sum {
		t.Fatalf("%d notes: %+v", n, noted)
	}
}

// A change to the trusted set within a session is recorded again, as is a
// person's terminal line that relies on it.
func TestTrustedStartupFilesAreRecordedWhenTheyChange(t *testing.T) {
	home := t.TempDir()
	prof := filepath.Join(home, ".zprofile")
	if err := os.WriteFile(prof, []byte("export A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	size, sum, _ := policy.StartupFileMark(prof)
	l, store := harnessIn(t, tempDir(t), nil, policy.ModeBypass, true)
	l.Policy.Home = func() (string, bool) { return home, true }
	l.Policy.StartupBaseline = map[string]policy.StartupFile{prof: {Size: size, SHA256: sum}}
	if _, err := l.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prof, []byte("export A=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ManualScreen("u1", "ls"); err != nil {
		t.Fatal(err)
	}
	evs, _ := store.Events("sess1")
	var sets []int
	for _, e := range evs {
		if e.Type == EvStartupTrusted {
			var p struct {
				Files []policy.TrustedStartupFile `json:"files"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			sets = append(sets, len(p.Files))
		}
	}
	if len(sets) != 2 || sets[0] != 1 || sets[1] != 0 {
		t.Fatalf("recorded sets: %v", sets)
	}
}
