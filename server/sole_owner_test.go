package server

import (
	"context"
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A server that shares its workspace and was started for one account refuses
// a session, chat or workbench, for any other.
func TestSoleOwnerRefusesAnotherAccount(t *testing.T) {
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), SoleOwner: "local:ann"})
	ctx := context.Background()
	if _, err := s.StartSession(ctx, StartSpec{Prompt: "hi", User: "local:bob", Tenant: "default"}); !errors.Is(err, errNotSoleOwner) {
		t.Fatalf("bob's chat: %v", err)
	}
	if _, err := s.openWorkbench(ctx, StartSpec{User: "local:bob", Tenant: "default"}); !errors.Is(err, errNotSoleOwner) {
		t.Fatalf("bob's workbench: %v", err)
	}
	if _, err := s.StartSession(ctx, StartSpec{Prompt: "hi", User: "local:ann", Tenant: "default"}); err != nil {
		t.Fatalf("ann's chat: %v", err)
	}
}
