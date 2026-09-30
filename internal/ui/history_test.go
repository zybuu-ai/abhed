package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What is written is redacted: a secret in a prompt never reaches the file.
func TestHistoryIsRedactedOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.jsonl")
	h := LoadHistory(path)
	h.SetRedact(func(s string) string { return strings.ReplaceAll(s, "CANARY-SECRET-42", "[redacted]") })
	h.Add("deploy with token CANARY-SECRET-42 please", true)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "CANARY-SECRET-42") || !strings.Contains(string(data), "[redacted]") {
		t.Fatalf("history on disk: %s", data)
	}
	// This session's Up still has what was typed.
	if e := h.Entries(); e[len(e)-1] != "deploy with token CANARY-SECRET-42 please" {
		t.Fatalf("in memory: %q", e)
	}
}

// A large paste is written as the prompt with its placeholder, and no entry
// on disk exceeds the cap.
func TestHistoryStoresPastesAsTheirLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.jsonl")
	h := LoadHistory(path)
	big := strings.Repeat("log line\n", 200_000)
	h.AddStored("see "+big, "see [Pasted text #1 +200000 lines]")
	h.AddStored(strings.Repeat("y", 200<<10), strings.Repeat("y", 200<<10))
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "log line") || !strings.Contains(string(data), "[Pasted text #1") {
		t.Fatalf("the paste was stored whole")
	}
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if len(l) > historyEntryMax+64 {
			t.Fatalf("an entry of %d bytes was stored", len(l))
		}
	}
}

// One over-long line in an older file is skipped; the entries after it load.
func TestHistorySkipsAnOverlongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.jsonl")
	body := `{"text":"before"}` + "\n" + `{"text":"` + strings.Repeat("x", historyLineMax+10) + `"}` + "\n" + `{"text":"after"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LoadHistory(path).Entries()
	if len(got) != 2 || got[0] != "before" || got[1] != "after" {
		t.Fatalf("loaded %d entries: %q…", len(got), got)
	}
}

// The file is never opened through a link, and a wider mode is narrowed.
func TestHistoryFileSafety(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte(`{"text":"planted"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "h.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	h := LoadHistory(link)
	if len(h.Entries()) != 0 {
		t.Fatalf("history was read through a link: %q", h.Entries())
	}
	h.Add("typed", true)
	data, _ := os.ReadFile(target)
	if strings.Contains(string(data), "typed") {
		t.Fatal("history was written through a link")
	}

	wide := filepath.Join(dir, "wide.jsonl")
	if err := os.WriteFile(wide, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	LoadHistory(wide).Add("x", true)
	if info, _ := os.Stat(wide); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}
