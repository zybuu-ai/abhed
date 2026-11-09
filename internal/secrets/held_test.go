package secrets

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Abhed's own values are set and read only through their own calls: a person
// cannot set or remove one by name, a command is never given one, and the
// names are not offered.
func TestHeldNames(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := s.Set("MCP_OAUTH_X", "planted-value"); err == nil {
		t.Fatal("a held name was set by hand")
	}
	if err := s.SetHeld("MCP_OAUTH_X", "kept-value-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("OTHER_TOKEN", "an-ordinary-one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("MCP_OAUTH_X"); err == nil {
		t.Fatal("a held name was removed piecemeal")
	}
	if v, ok, err := s.HeldValue("MCP_OAUTH_X"); !ok || err != nil || v != "kept-value-1" {
		t.Fatal(v, ok, err)
	}
	if _, err := s.Env([]string{"MCP_OAUTH_X"}); err == nil {
		t.Fatal("a held value was handed out")
	}
	if got := s.Offered(); len(got) != 1 || got[0] != "OTHER_TOKEN" {
		t.Fatalf("offered %v", got)
	}
	unlock, err := s.HeldLock(context.Background(), "MCP_OAUTH_X")
	if err != nil {
		t.Fatal(err)
	}
	// A second taker waits only as long as its context: a holder that never
	// lets go does not hold it forever.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if _, err := s.HeldLock(ctx, "MCP_OAUTH_X"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held lock was taken twice, or waited past its context: %v", err)
	}
	cancel()
	unlock()
	again, err := s.HeldLock(context.Background(), "MCP_OAUTH_X")
	if err != nil {
		t.Fatal(err)
	}
	again()
	if _, err := s.HeldLock(context.Background(), "OTHER_TOKEN"); err == nil {
		t.Fatal("a lock on an ordinary name")
	}
}
