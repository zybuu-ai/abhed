package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// failingStore is the memory store with appends refused while fail names the event type ("*" for any).
type failingStore struct {
	*agent.MemStore
	fail atomic.Value
}

func (f *failingStore) Append(ev agent.Event) error {
	if t, _ := f.fail.Load().(string); t == "*" || t == string(ev.Type) {
		return errors.New("disk full")
	}
	return f.MemStore.Append(ev)
}

// A chat whose session.started cannot be recorded is refused: 500, nothing
// left running, and no model call made for a session with no record.
func TestChatWhoseStartCannotBeRecordedIsRefused(t *testing.T) {
	m := newModelServer(t, "hi")
	cfg := config.Default()
	cfg.Model.Default = "a"
	cfg.Model.Providers = map[string]config.ProviderConfig{
		"a": {Type: "openai-compatible", BaseURL: m.url, Model: "model-a"},
	}
	def, err := cfg.Model.Providers["a"].Adapter()
	if err != nil {
		t.Fatal(err)
	}
	st := &failingStore{MemStore: agent.NewMemStore()}
	st.fail.Store(string(agent.EvSessionStarted))
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: def,
		Registry: tools.NewRegistry(tools.Read{}), Store: st})
	if w := call(t, s, "POST", "/v1/sessions", `{"prompt":"one"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("an unrecorded start: %d %s, want 500", w.Code, w.Body.String())
	}
	s.mu.Lock()
	running := len(s.running)
	s.mu.Unlock()
	if running != 0 || m.calls.Load() != 0 {
		t.Fatalf("%d sessions left running and %d model calls after a refused start", running, m.calls.Load())
	}
}

// An Explorer change a write rule refuses, whose refusal the record then
// refuses too, answers 500 as an unrecorded change does, not a silent 403.
func TestExplorerRefusalTheRecordRefusesAnswers500(t *testing.T) {
	cfg := config.Default()
	cfg.Permissions.Deny = append(cfg.Permissions.Deny, "write(**/frozen/**)")
	dir := t.TempDir()
	st := &failingStore{MemStore: agent.NewMemStore()}
	s := New(Options{Workspace: dir, Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{}), Store: st})
	id := sessionOf(t, call(t, s, "POST", "/v1/sessions", `{"workbench":true}`))
	st.fail.Store("*")
	w := call(t, s, "POST", "/v1/sessions/"+id+"/folder", `{"path":"frozen/x"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a refusal the record refused: %d %s, want 500", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "frozen")); err == nil {
		t.Fatal("the refused folder was made")
	}
}
