package toolset

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// changes are the config.changed events a session recorded, and their trust.
func changes(t *testing.T, store *agent.MemStore, id string) ([]agent.ConfigChanged, []agent.Trust) {
	t.Helper()
	evs, err := store.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	var out []agent.ConfigChanged
	var trust []agent.Trust
	for _, ev := range evs {
		if ev.Type == agent.EvConfigChanged {
			var c agent.ConfigChanged
			if err := json.Unmarshal(ev.Payload, &c); err != nil {
				t.Fatal(err)
			}
			out = append(out, c)
			trust = append(trust, ev.Trust)
		}
	}
	return out, trust
}

// The change log's entries reach the next session's record once each,
// escaped and untrusted; a partial last line waits, and a shrunk log is read
// from its start.
func TestRecordConfigChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := filepath.Join(home, ".abhed", "config-changes.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	store := agent.NewMemStore()
	session := func(id string) ([]agent.ConfigChanged, []agent.Trust) {
		if err := RecordConfigChanges(agent.NewRecorder(store, id, "")); err != nil {
			t.Fatal(err)
		}
		return changes(t, store, id)
	}
	if got, _ := session("s0"); len(got) != 0 {
		t.Fatalf("no log: %v", got)
	}
	write(`{"time":"2026-11-09T05:35:00Z","by":"mcp add -confirm-fd","key":"mcp.servers","name":"issues‮","kind":"stdio","sha256":"ab"}` + "\n" + `not json` + "\n" + `{"by":"mcp add","key":"mcp.servers","name":"part`)
	got, trust := session("s1")
	if len(got) != 1 || got[0].By != "mcp add -confirm-fd" || got[0].Name != "issues⟨U+202E⟩" || got[0].Kind != "stdio" || trust[0] != agent.Untrusted {
		t.Fatalf("first: %+v %v", got, trust)
	}
	if got, _ := session("s2"); len(got) != 0 {
		t.Fatalf("recorded again: %v", got)
	}
	write(`ial","refused":"inside an agent's command"}` + "\n")
	if got, _ := session("s3"); len(got) != 1 || got[0].Name != "partial" || got[0].Refused == "" {
		t.Fatalf("the finished line: %+v", got)
	}
	// A shrunk log (rotated by hand) is read from its start.
	if err := os.WriteFile(logPath, []byte(`{"by":"mcp remove","key":"mcp.servers","name":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := session("s4"); len(got) != 1 || !strings.Contains(got[0].By, "remove") {
		t.Fatalf("after a shrink: %+v", got)
	}
}

// More than a session takes: it stops at 200, says how many wait, and the
// rest, a real change last, land in the next sessions.
func TestRecordConfigChangesCarriesOverPastTheCap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := filepath.Join(home, ".abhed", "config-changes.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < 250; i++ {
		b.WriteString(`{"by":"mcp add","key":"mcp.servers","name":"spam","refused":"mcp.denied"}` + "\n")
	}
	b.WriteString(`{"by":"mcp add -confirm-fd","key":"mcp.servers","name":"real","kind":"stdio","sha256":"ab"}` + "\n")
	if err := os.WriteFile(logPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	store := agent.NewMemStore()
	got := map[string][]agent.ConfigChanged{}
	for _, id := range []string{"s1", "s2"} {
		if err := RecordConfigChanges(agent.NewRecorder(store, id, "")); err != nil {
			t.Fatal(err)
		}
		got[id], _ = changes(t, store, id)
	}
	first := got["s1"]
	if len(first) != 201 || first[200].Pending != 51 || first[199].Name != "spam" {
		t.Fatalf("first session: %d entries, last %+v", len(first), first[len(first)-1])
	}
	second := got["s2"]
	if len(second) != 51 || second[50].Name != "real" || second[50].Refused != "" {
		t.Fatalf("second session: %d entries, last %+v", len(second), second[len(second)-1])
	}
	if err := RecordConfigChanges(agent.NewRecorder(store, "s3", "")); err != nil {
		t.Fatal(err)
	}
	if third, _ := changes(t, store, "s3"); len(third) != 0 {
		t.Fatalf("recorded again: %d", len(third))
	}
}

// A log replaced by hand with one longer than the old mark is read from its
// start, not mid-line.
func TestRecordConfigChangesSeesARotatedLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := filepath.Join(home, ".abhed", "config-changes.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(`{"by":"mcp add","key":"mcp.servers","name":"a"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := agent.NewMemStore()
	if err := RecordConfigChanges(agent.NewRecorder(store, "s1", "")); err != nil {
		t.Fatal(err)
	}
	rotated := `{"by":"mcp add","key":"mcp.servers","name":"rotated-first-entry-longer"}` + "\n" + `{"by":"mcp remove","key":"mcp.servers","name":"b"}` + "\n"
	if err := os.WriteFile(logPath, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecordConfigChanges(agent.NewRecorder(store, "s2", "")); err != nil {
		t.Fatal(err)
	}
	if got, _ := changes(t, store, "s2"); len(got) != 2 || got[0].Name != "rotated-first-entry-longer" {
		t.Fatalf("rotated: %+v", got)
	}
}

// A long field is cut on a character boundary.
func TestClipKeepsCharactersWhole(t *testing.T) {
	s := strings.Repeat("a", 255) + "é" + "tail"
	c := clip(s)
	if !utf8.ValidString(c) || !strings.HasSuffix(c, "a…") {
		t.Fatalf("%q", c[250:])
	}
}

// A line longer than the window is skipped to its end with an entry saying
// so, and the log goes on: a real add after it is recorded.
func TestRecordConfigChangesSkipsAnOversizeLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := filepath.Join(home, ".abhed", "config-changes.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	huge := `{"by":"mcp add","key":"mcp.servers","name":"` + strings.Repeat("x", configChangesReadMax+10) + `"}` + "\n"
	real := `{"by":"mcp add -confirm-fd","key":"mcp.servers","name":"real"}` + "\n"
	if err := os.WriteFile(logPath, []byte(huge+real), 0o600); err != nil {
		t.Fatal(err)
	}
	store := agent.NewMemStore()
	var all []agent.ConfigChanged
	for _, id := range []string{"s1", "s2", "s3"} {
		if err := RecordConfigChanges(agent.NewRecorder(store, id, "")); err != nil {
			t.Fatal(err)
		}
		got, _ := changes(t, store, id)
		all = append(all, got...)
	}
	if len(all) != 2 || all[0].Refused != "a line over 1 MiB was skipped" || all[0].By != "abhed" || all[1].Name != "real" {
		t.Fatalf("%+v", all)
	}
}
