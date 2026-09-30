package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// The editor's draft lives in a folder only its owner can open, under the
// home folder rather than the shared temporary one; it is never read back
// through a link; and drafts a crash left behind are removed.
func TestDraftFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	name, err := newDraft("my prompt")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(name)
	if dir != filepath.Join(home, ".abhed", "drafts") {
		t.Fatalf("draft in %s", dir)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("draft folder mode %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(name); info.Mode().Perm() != 0o600 {
		t.Fatalf("draft mode %v", info.Mode().Perm())
	}
	if got, _ := readDraft(name); got != "my prompt" {
		t.Fatalf("read %q", got)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(secret, []byte("not for the prompt"), 0o600)
	_ = os.Remove(name)
	if err := os.Symlink(secret, name); err != nil {
		t.Skip(err)
	}
	if got, err := readDraft(name); err == nil {
		t.Fatalf("a draft swapped for a link was read: %q", got)
	}
	fresh, _ := newDraft("another session's, open now")
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(name, old, old)
	_ = os.Remove(name)
	stale, _ := newDraft("left by a crash")
	_ = os.Chtimes(stale, old, old)
	cleanDrafts()
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("a stale draft was left")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a draft in use was removed")
	}
}
