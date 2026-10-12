package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/store"
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

// rowStore is failingStore with session rows, as a durable store keeps them.
type rowStore struct {
	failingStore
	mu   sync.Mutex
	rows map[string]store.SessionRecord
}

func (r *rowStore) CreateSession(_ context.Context, rec store.SessionRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows[rec.ID] = rec
	return nil
}

func (r *rowStore) ListSessions(context.Context, int) ([]store.SessionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.SessionRecord, 0, len(r.rows))
	for _, rec := range r.rows {
		out = append(out, rec)
	}
	return out, nil
}

func (r *rowStore) DeleteSession(id string) error {
	r.mu.Lock()
	delete(r.rows, id)
	r.mu.Unlock()
	return r.MemStore.DeleteSession(id)
}

// A start refused after its row was written, because its first events
// could not be recorded, forgets the row: it is not listed as a session.
func TestRefusedStartLeavesNoSessionRow(t *testing.T) {
	for _, ev := range []agent.EventType{agent.EvSessionStarted, agent.EvPolicyLayer} {
		t.Run(string(ev), func(t *testing.T) {
			st := &rowStore{failingStore: failingStore{MemStore: agent.NewMemStore()}, rows: map[string]store.SessionRecord{}}
			st.fail.Store(string(ev))
			s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
				Registry: tools.NewRegistry(tools.Read{}), Store: st,
				SessionPolicy: func(context.Context, SessionPolicyRequest) (*PolicyLayer, error) {
					return &PolicyLayer{Name: "team", Deny: []string{"bash(rm*)"}}, nil
				}})
			if w := call(t, s, "POST", "/v1/sessions", `{"prompt":"one"}`); w.Code < 500 {
				t.Fatalf("a start whose %s was not recorded: %d %s", ev, w.Code, w.Body.String())
			}
			st.mu.Lock()
			n := len(st.rows)
			st.mu.Unlock()
			if n != 0 {
				t.Fatalf("%d session rows left by a refused start", n)
			}
		})
	}
}
