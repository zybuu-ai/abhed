package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A start the store refuses answers a fixed message: the store's own error
// goes to the log, not to the caller.
func TestStartRefusalKeepsTheStoreErrorOut(t *testing.T) {
	st := &failingStore{MemStore: agent.NewMemStore()}
	st.fail.Store(string(agent.EvSessionStarted))
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Store: st})
	w := call(t, s, "POST", "/v1/sessions", `{"prompt":"one"}`)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "disk full") {
		t.Fatalf("a refused start = %d %s, want 500 without the store's error", w.Code, w.Body)
	}
}

// A new session, chat or workbench, is refused with 503 while the operator's
// secrets store cannot be loaded, rather than run with its record withheld;
// once the store loads again, sessions start.
func TestNewSessionRefusedWhileSecretsUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := secrets.Open(path).Set("SOME_TOKEN", "fake-value-0042aa"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secrets.EnvFile, path)
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Redact: secrets.Open(path).Live()})
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"prompt":"one"}`, `{"workbench":true}`} {
		w := call(t, s, "POST", "/v1/sessions", body)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" ||
			!strings.Contains(w.Body.String(), "secrets store") {
			t.Fatalf("%s with an unreadable store = %d %s", body, w.Code, w.Body)
		}
	}
	s.mu.RLock()
	n := len(s.running)
	s.mu.RUnlock()
	if n != 0 {
		t.Fatalf("%d sessions running after refusals", n)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if w := call(t, s, "POST", "/v1/sessions", `{"workbench":true}`); w.Code != http.StatusAccepted {
		t.Fatalf("after the store was fixed = %d %s", w.Code, w.Body)
	}
}

// A client id is checked when a session is started from Go, as on the route.
func TestStartSessionChecksTheClientID(t *testing.T) {
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{})})
	for _, id := range []string{strings.Repeat("a", 65), "has space", "semi;colon"} {
		if _, err := s.StartSession(context.Background(), StartSpec{Prompt: "hi", ClientID: id}); !errors.Is(err, errBadClientID) {
			t.Fatalf("client id %q: %v", id, err)
		}
	}
	if _, err := s.StartSession(context.Background(), StartSpec{Prompt: "hi", ClientID: "ok-id_1"}); err != nil {
		t.Fatalf("a plain client id: %v", err)
	}
}
