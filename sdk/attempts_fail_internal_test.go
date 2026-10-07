package abhed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/embedded"
)

// refusingStore cannot record a config.refused event, and notes a release.
type refusingStore struct {
	*agent.MemStore
	released *[]string
}

func (s refusingStore) Append(ev agent.Event) error {
	if ev.Type == agent.EvConfigRefused {
		return errors.New("disk full")
	}
	return s.MemStore.Append(ev)
}

func (s refusingStore) Release(id string) error {
	*s.released = append(*s.released, id)
	return nil
}

// A surface's agent whose configuration attempts cannot be recorded is not
// built, as the command line refuses to run, and the record New opened for
// it is let go, so another process may take the session.
func TestNewFailsWhenTheAttemptsCannotBeRecorded(t *testing.T) {
	managedFile(t, "{}")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	on := `{"web_search":{"enabled":true,"provider":"searxng","base_url":"http://127.0.0.1:9/sink"}}`
	if err := os.WriteFile(filepath.Join(dir, ".abhed", "config.json"), []byte(on), 0o600); err != nil {
		t.Fatal(err)
	}
	var released []string
	ctx := embedded.With(context.Background(), embedded.Settings{ID: "s-attempts", Surface: "test"})
	a, err := New(ctx, Options{
		Workspace: dir, ConfigDir: dir, WorkspaceTrust: config.TrustGranted, Provider: testProvider,
		Store: refusingStore{MemStore: agent.NewMemStore(), released: &released},
	})
	if err == nil {
		a.Close()
		t.Fatal("New built an agent whose configuration attempts were not recorded")
	}
	if !strings.Contains(err.Error(), "recording the configuration attempts") {
		t.Fatalf("New failed for another reason: %v", err)
	}
	if len(released) != 1 || released[0] != "s-attempts" {
		t.Fatalf("the record was not let go: %v", released)
	}
}
