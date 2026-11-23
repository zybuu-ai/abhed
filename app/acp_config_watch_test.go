package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// A change is sorted into what an open chat can take (models added, a new
// default) and what waits for a new chat (everything else, a provider's own
// fields included).
func TestDiffUserConfig(t *testing.T) {
	before := `{"model":{"default":"a","providers":{"a":{"type":"x","model":"m"}}},"permissions":{"mode":"default"}}`
	after := `{"model":{"default":"b","providers":{"a":{"type":"x","model":"m2"},"b":{"type":"y","model":"n"},"c":{"type":"z","model":"o"}}},"permissions":{"mode":"bypass"},"sandbox":{"allow_network":true}}`
	ch := diffUserConfig([]byte(before), []byte(after))
	if strings.Join(ch.Models, ",") != "b,c" || ch.Default != "b" || strings.Join(ch.NewChats, ",") != "model.providers.a,permissions.mode,sandbox.allow_network" {
		t.Fatalf("%+v", ch)
	}
	if !strings.Contains(ch.message(), "applies to new chats") || !strings.Contains(ch.message(), "never applied to it") {
		t.Fatalf("message %q", ch.message())
	}
	if !diffUserConfig([]byte(before), []byte(before)).empty() {
		t.Fatal("no change read as one")
	}
}

// Studio is told when the person's file changes while a chat is open; the
// chat takes the added models and, asked, the new default, and nothing else:
// its permissions stay as it started.
func TestStudioConfigChangedAndApplied(t *testing.T) {
	was := configWatchEvery
	configWatchEvery = 50 * time.Millisecond
	t.Cleanup(func() { configWatchEvery = was })
	r := newStudioRig(t, "", say("ok"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.cl.conn.ctx = ctx // the watch lasts as long as the connection
	file := filepath.Join(os.Getenv("HOME"), ".abhed", "config.json")
	id := r.open()
	from := r.cl.mark()
	prov := stubProvider(r.model.URL, "m-2")
	body := `{"model":{"default":"fresh","providers":{"m":` + stubProvider(r.model.URL, "m-1") + `,"fresh":` + prov + `}},"permissions":{"mode":"bypass"}}`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	note := r.cl.waitFor(from, "the change notice", func(m rpcMessage) bool { return m.Method == "_abhed/config/changed" })
	p := string(note.Params)
	if !strings.Contains(p, `"models":["fresh"]`) || !strings.Contains(p, `"default":"fresh"`) || !strings.Contains(p, "permissions.mode") ||
		!strings.Contains(p, `"canApply":true`) || !strings.Contains(p, id) {
		t.Fatalf("notice %s", p)
	}
	var got struct {
		Added      []string `json:"added"`
		SwitchedTo string   `json:"switchedTo"`
	}
	r.cl.ok("_abhed/config/apply", map[string]any{"sessionId": id, "switchDefault": true}, &got)
	if strings.Join(got.Added, ",") != "fresh" || got.SwitchedTo != "fresh" {
		t.Fatalf("applied %+v", got)
	}
	if applied, _ := r.recorded(id, agent.EvConfigApplied); len(applied) != 1 {
		t.Fatalf("recorded %v", applied)
	}
	// The mode the file now names is not this chat's.
	if s := r.cl.conn.openSessions(); len(s) == 0 || s[0].parts.Loop.Policy.Mode == "bypass" {
		t.Fatal("the open chat took the widened mode")
	}
}

// At the terminal, a change is said after the task, and /config apply
// offers the added models in the session.
func TestCLIConfigChanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := filepath.Join(home, ".abhed", "config.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"permissions":{"mode":"default"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	e, _ := configEnv(cfg, "")
	e.st.workspace = t.TempDir()
	if n := e.st.configChangedNote(); n != "" {
		t.Fatalf("the first look said %q", n)
	}
	if err := os.WriteFile(file, []byte(`{"permissions":{"mode":"auto"},"model":{"providers":{"extra":{"type":"openai-compatible","base_url":"http://127.0.0.1:9/v1","model":"x"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	n := e.st.configChangedNote()
	if !strings.Contains(n, "permissions.mode") || !strings.Contains(n, "/config apply") {
		t.Fatalf("note %q", n)
	}
	if e.st.configChangedNote() != "" {
		t.Fatal("said twice")
	}
	if err := configApplyCLI(e); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.st.appCfg.Model.Providers["extra"]; !ok || e.st.appCfg.Permissions.Mode == "auto" {
		t.Fatalf("applied %+v", e.st.appCfg.Permissions)
	}
}
