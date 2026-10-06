package abhed

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// closingSandbox notes when it is closed and fails as a fence that found
// state planted does.
type closingSandbox struct{ log *[]string }

func (closingSandbox) Tier() sandbox.Tier                                { return sandbox.TierFence }
func (closingSandbox) Available() (bool, string)                         { return true, "" }
func (closingSandbox) Describe() string                                  { return "test" }
func (closingSandbox) Command(context.Context, string, string) *exec.Cmd { return nil }
func (s closingSandbox) Close() error {
	*s.log = append(*s.log, "sandbox")
	return errors.New("fence: a command made .abhed in the workspace")
}

// releasingStore notes when the agent lets its record go.
type releasingStore struct {
	agent.Store
	log *[]string
}

func (s releasingStore) Release(string) error {
	*s.log = append(*s.log, "record")
	return nil
}

// Close closes the sandbox while the record is still held, so what the
// fence finds then can be recorded, and passes its error to Warn.
func TestCloseEndsTheSandboxBeforeTheRecord(t *testing.T) {
	var log, warned []string
	a, err := New(context.Background(), Options{
		Workspace: t.TempDir(),
		Provider:  &Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}, // never reached
		Warn:      func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	a.sandbox = closingSandbox{log: &log}
	a.store = releasingStore{Store: a.store, log: &log}
	a.Close()
	if !slices.Equal(log, []string{"sandbox", "record"}) {
		t.Fatalf("closed in the order %v", log)
	}
	if len(warned) != 1 || warned[0] != "abhed: fence: a command made .abhed in the workspace" {
		t.Fatalf("warned %q", warned)
	}
}
