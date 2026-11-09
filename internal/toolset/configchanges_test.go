package toolset

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
